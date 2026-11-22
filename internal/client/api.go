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
	"mime"
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
	msg := fmt.Sprintf("server returned %d %s: %s", e.Status, e.Code, e.Message)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry in %s)", e.RetryAfter)
	}
	return msg
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
				// For a file: the server answers the headers before the body
				// is sent (SendFile sets Expect), so a refusal costs nothing.
				ExpectContinueTimeout: 5 * time.Second,
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

// NormalizeFingerprint turns a certificate fingerprint as a person or a tool
// writes it — "sha256:AB:CD:…", "ab cd …", plain hex — into the form the
// registry stores: 64 lowercase hex characters. It returns "" for anything
// that is not a SHA-256.
func NormalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.NewReplacer(":", "", " ", "", "-", "").Replace(s)
	if len(s) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return s
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
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError reads a non-2xx response into an APIError. A body that is not the
// server's JSON (a route an older server does not have) comes back under the
// code "http_error".
func apiError(resp *http.Response) *APIError {
	var eb proto.ErrorBody
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	_ = json.Unmarshal(data, &eb)
	if eb.Error == "" {
		eb.Error = "http_error"
		eb.Message = strings.TrimSpace(string(data))
	}
	ae := &APIError{Status: resp.StatusCode, Code: eb.Error, Message: eb.Message}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			ae.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return ae
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

// SendFile posts a message that carries a file: body is its caption (it may
// be empty), name what the file is called. Who is sending goes in the query
// and the bytes are the request body — see the proto package.
func (c *Conn) SendFile(room, pid, body, name string, data []byte) (proto.SendResponse, error) {
	var out proto.SendResponse
	q := url.Values{}
	q.Set("pid", pid)
	q.Set("name", name)
	if body != "" {
		q.Set("body", body)
	}
	req, err := http.NewRequest("POST", c.base+roomPath(room, "/files")+"?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	req.Header.Set("Content-Type", "application/octet-stream")
	// The server can refuse on the request's head alone; with this it is asked
	// before the bytes are sent rather than after.
	req.Header.Set("Expect", "100-continue")
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out, apiError(resp)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// File fetches a file a message carried: its bytes, and the name and type
// the server holds for it. limit bounds what is read; a file larger than it
// is an error, not a truncation.
func (c *Conn) File(room, id string, limit int64) (proto.File, []byte, error) {
	req, err := http.NewRequest("GET", c.base+roomPath(room, "/files/"+url.PathEscape(id)), nil)
	if err != nil {
		return proto.File{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return proto.File{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return proto.File{}, nil, apiError(resp)
	}
	if resp.ContentLength > limit {
		return proto.File{}, nil, fmt.Errorf("the file is %d bytes, more than the %d this client will take", resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return proto.File{}, nil, fmt.Errorf("the file did not arrive whole: %w", err)
	}
	if int64(len(data)) > limit {
		return proto.File{}, nil, fmt.Errorf("the file is more than the %d bytes this client will take", limit)
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return proto.File{}, nil, fmt.Errorf("the file did not arrive whole: %d of %d bytes", len(data), resp.ContentLength)
	}
	meta := proto.File{ID: id, Size: int64(len(data)), Type: resp.Header.Get("X-Crosstalk-File-Type")}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		meta.Name = params["filename"]
	}
	return meta, data, nil
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
