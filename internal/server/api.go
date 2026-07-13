package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// API wraps a Hub with the HTTP surface: bearer-token auth, proto-version
// enforcement, and the long-poll events endpoint.
type API struct {
	Hub   *Hub
	Token string

	failMu   sync.Mutex
	failures map[string]*tokenBucket // per-source-IP auth-failure limiter
}

func NewAPI(hub *Hub, token string) *API {
	return &API{Hub: hub, Token: token, failures: make(map[string]*tokenBucket)}
}

// Handler returns the full route table. Distribution endpoints (/install.sh,
// /bin/*) land here in Phase 3; /version is already served unauthenticated
// because `crosstalk update` and the install script need it before auth.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, proto.VersionResponse{Version: proto.Version, Build: proto.Build, Proto: proto.ProtoVersion})
	})
	mux.HandleFunc("GET /install.sh", handleInstallScript)
	mux.HandleFunc("GET /bin/{file}", handleBinary)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/rooms/{room}/join", a.handleJoin)
	api.HandleFunc("POST /api/rooms/{room}/messages", a.handleSend)
	api.HandleFunc("GET /api/rooms/{room}/events", a.handleEvents)
	api.HandleFunc("GET /api/rooms/{room}/history", a.handleHistory)
	api.HandleFunc("GET /api/rooms/{room}/participants", func(w http.ResponseWriter, r *http.Request) {
		ps, err := a.Hub.Participants(r.PathValue("room"))
		if err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ps)
	})
	api.HandleFunc("POST /api/rooms/{room}/task", a.handleTask)
	api.HandleFunc("POST /api/rooms/{room}/leave", a.handleLeave)
	api.HandleFunc("GET /api/rooms", a.handleRooms)
	api.HandleFunc("GET /api/rooms/{$}", a.handleRooms)

	mux.Handle("/api/", a.authenticate(api))
	return mux
}

// authenticate enforces the proto-version header and the global bearer
// token, rate-limiting and logging failed attempts per source address.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Crosstalk-Proto"); v != proto.ProtoVersion {
			writeErr(w, http.StatusUpgradeRequired, proto.ErrProtoUnsupported,
				fmt.Sprintf("this server speaks proto %s, client sent %q: run `crosstalk update`", proto.ProtoVersion, v))
			return
		}

		ip := sourceIP(r)
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
			if !a.allowAuthFailure(ip) {
				writeErr(w, http.StatusTooManyRequests, proto.ErrRateLimited, "too many failed auth attempts")
				return
			}
			log.Printf("auth: rejected request from %s (%s %s)", ip, r.Method, r.URL.Path)
			writeErr(w, http.StatusUnauthorized, proto.ErrUnauthorized, "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowAuthFailure permits up to ~10 failed attempts per minute per IP.
func (a *API) allowAuthFailure(ip string) bool {
	a.failMu.Lock()
	defer a.failMu.Unlock()
	if len(a.failures) > 10000 { // bound the map; hostile scans reset each other
		a.failures = make(map[string]*tokenBucket)
	}
	b := a.failures[ip]
	if b == nil {
		b = &tokenBucket{}
		a.failures[ip] = b
	}
	_, ok := b.take(time.Now(), 10.0/60.0, 10)
	return ok
}

func (a *API) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req proto.JoinRequest
	if !readJSON(w, r, &req) {
		return
	}
	resp, err := a.Hub.Join(r.PathValue("room"), proto.Identity{
		Hostname: req.Hostname, Name: req.Name, Mode: req.Mode, Task: req.Task,
	}, sourceIP(r))
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	log.Printf("join: %s/%s (%s) -> room %q pid %s from %s",
		req.Hostname, req.Name, req.Mode, r.PathValue("room"), resp.PID, sourceIP(r))
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleSend(w http.ResponseWriter, r *http.Request) {
	var req proto.SendRequest
	if !readJSON(w, r, &req) {
		return
	}
	ev, err := a.Hub.Send(r.PathValue("room"), req.PID, req.Body, req.Kind)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SendResponse{ID: ev.ID})
}

func (a *API) handleTask(w http.ResponseWriter, r *http.Request) {
	var req proto.TaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.Hub.SetTask(r.PathValue("room"), req.PID, req.Task); err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *API) handleLeave(w http.ResponseWriter, r *http.Request) {
	var req proto.LeaveRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.Hub.Leave(r.PathValue("room"), req.PID); err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// handleEvents is the sole live read path: return immediately if events
// newer than the cursor exist, otherwise park until something arrives or
// the hold expires. A superseded poll (a newer poll for the same pid) wakes,
// finds nothing new, and returns an empty list.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	q := r.URL.Query()
	pid := q.Get("pid")
	after := q.Get("after")

	wait := 0 * time.Second
	if s := q.Get("wait"); s != "" {
		secs, err := strconv.Atoi(s)
		if err != nil || secs < 0 {
			writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "wait must be a non-negative integer (seconds)")
			return
		}
		wait = time.Duration(secs) * time.Second
	}
	if wait > a.Hub.MaxWait() {
		wait = a.Hub.MaxWait()
	}
	deadline := time.Now().Add(wait)

	evs, err := a.Hub.EventsAfter(room, pid, after)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	if len(evs) > 0 || wait == 0 {
		writeJSON(w, http.StatusOK, proto.EventsResponse{Events: evs})
		return
	}

	ch, err := a.Hub.Park(room, pid)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	// Re-check after parking: an event appended between the check above and
	// Park would have woken only already-parked waiters.
	evs, err = a.Hub.EventsAfter(room, pid, after)
	if err != nil {
		a.Hub.Unpark(room, pid, ch)
		writeAPIErr(w, err)
		return
	}
	if len(evs) > 0 {
		a.Hub.Unpark(room, pid, ch)
		writeJSON(w, http.StatusOK, proto.EventsResponse{Events: evs})
		return
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ch:
		// Woken: events arrived, the room closed, or a newer poll for this
		// pid superseded us — the re-check resolves which, and a superseded
		// poll simply returns an empty list. Deliberately no re-park here:
		// two competing polls looping on supersede would livelock.
		evs, err := a.Hub.EventsAfter(room, pid, after)
		if err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.EventsResponse{Events: evs})
	case <-timer.C:
		a.Hub.Unpark(room, pid, ch)
		writeJSON(w, http.StatusOK, proto.EventsResponse{Events: nil})
	case <-r.Context().Done():
		a.Hub.Unpark(room, pid, ch)
	}
}

func (a *API) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	evs, err := a.Hub.History(r.PathValue("room"), q.Get("before"), limit)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proto.EventsResponse{Events: evs})
}

func (a *API) handleRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.Hub.Rooms())
}

func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// readJSON decodes a request body capped at 64 KiB (comfortably above the
// 16 KiB message cap), writing the error response itself on failure.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, proto.ErrTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "malformed JSON body: "+err.Error())
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, proto.ErrorBody{Error: code, Message: msg})
}

func writeAPIErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(ae.RetryAfter.Seconds()+0.999)))
	}
	writeErr(w, ae.Status, ae.Code, ae.Message)
}
