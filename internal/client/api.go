package client

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// APIError is a non-2xx response with its machine-readable code, so callers
// can branch on proto.ErrPIDGone vs proto.ErrCursorGone etc.
type APIError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("server returned %d %s: %s", e.Status, e.Code, e.Message)
}

// Conn is a client for one registered server, verifying the pinned
// certificate fingerprint on every request.
type Conn struct {
	base  string
	token string
	http  *http.Client
}

// NewConn builds a connection that accepts exactly the certificate whose
// SHA-256 matches the registration-time pin, and nothing else.
func NewConn(e ServerEntry) *Conn {
	return &Conn{
		base:  fmt.Sprintf("https://%s", joinHostPort(e.Addr, e.Port)),
		token: e.Token,
		http: &http.Client{
			// No overall client timeout: the events long-poll legitimately
			// holds ~30s. Per-request bounds come from the wait parameter.
			Transport: &http.Transport{
				TLSClientConfig:       pinnedTLSConfig(e.Fingerprint),
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
	}
}

func joinHostPort(addr string, port int) string {
	return fmt.Sprintf("%s:%d", addr, port) // IPv6 literals: user supplies [::1]
}

func pinnedTLSConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		// The self-signed cert can't chain to a CA; identity is the pin.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != fingerprint {
				return fmt.Errorf("server certificate does not match the pinned fingerprint from registration — if the server was reinstalled, re-run `crosstalk register`")
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
}

// FetchFingerprint connects without a pin and returns the SHA-256 of the
// certificate the server presents: the trust-on-first-use moment.
func FetchFingerprint(addr string, port int) (string, error) {
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	conn, err := d.Dial("tcp", joinHostPort(addr, port))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("server presented no certificate")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Conn) do(method, path string, query url.Values, body, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		var eb proto.ErrorBody
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = json.Unmarshal(data, &eb)
		if eb.Error == "" {
			eb.Error = "http_error"
			eb.Message = string(data)
		}
		ae := &APIError{Status: resp.StatusCode, Code: eb.Error, Message: eb.Message}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				ae.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return ae
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func roomPath(room, suffix string) string {
	return "/api/rooms/" + url.PathEscape(room) + suffix
}

func (c *Conn) Join(room string, req proto.JoinRequest) (proto.JoinResponse, error) {
	var out proto.JoinResponse
	err := c.do("POST", roomPath(room, "/join"), nil, req, &out)
	return out, err
}

func (c *Conn) Send(room, pid, body, kind string) (proto.SendResponse, error) {
	var out proto.SendResponse
	err := c.do("POST", roomPath(room, "/messages"), nil, proto.SendRequest{PID: pid, Body: body, Kind: kind}, &out)
	return out, err
}

func (c *Conn) SetTask(room, pid, task string) error {
	return c.do("POST", roomPath(room, "/task"), nil, proto.TaskRequest{PID: pid, Task: task}, nil)
}

func (c *Conn) Leave(room, pid string) error {
	return c.do("POST", roomPath(room, "/leave"), nil, proto.LeaveRequest{PID: pid}, nil)
}

// Events performs one long-poll cycle. wait of zero returns immediately.
func (c *Conn) Events(room, pid, after string, wait time.Duration) ([]proto.Event, error) {
	q := url.Values{}
	q.Set("pid", pid)
	if after != "" {
		q.Set("after", after)
	}
	q.Set("wait", strconv.Itoa(int(wait.Seconds())))
	var out proto.EventsResponse
	err := c.do("GET", roomPath(room, "/events"), q, nil, &out)
	return out.Events, err
}

func (c *Conn) History(room, before string, limit int) ([]proto.Event, error) {
	q := url.Values{}
	if before != "" {
		q.Set("before", before)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out proto.EventsResponse
	err := c.do("GET", roomPath(room, "/history"), q, nil, &out)
	return out.Events, err
}

func (c *Conn) Participants(room string) ([]proto.ParticipantInfo, error) {
	var out []proto.ParticipantInfo
	err := c.do("GET", roomPath(room, "/participants"), nil, nil, &out)
	return out, err
}

func (c *Conn) Rooms() ([]proto.RoomInfo, error) {
	var out []proto.RoomInfo
	err := c.do("GET", "/api/rooms", nil, nil, &out)
	return out, err
}

// Download fetches a raw asset (client binary, checksum manifest) over the
// pinned connection.
func (c *Conn) Download(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("server returned %d for %s: %s", resp.StatusCode, path, strings.TrimSpace(string(data)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256*1024*1024))
}

func (c *Conn) Version() (proto.VersionResponse, error) {
	var out proto.VersionResponse
	err := c.do("GET", "/version", nil, nil, &out)
	return out, err
}
