package server

import (
	"context"
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

	// The operator: optional, and off when OperatorToken is empty.
	//
	// The fleet token is one secret shared by every machine, and with it a
	// client declares its own hostname. That is fine for what it protects
	// against (a confused agent), and not enough for one identity that has to
	// mean something: a person signed in to a front end that speaks for them —
	// lv_hub hosts crosstalk and joins rooms as hub/<account>. So that nothing
	// holding only the fleet token can speak under that hostname, a join that
	// declares OperatorHostname must present OperatorToken, and the pid it
	// gets works only with that token.
	//
	// The operator token is accepted wherever the fleet token is. It is not an
	// admin credential: beyond joining under its hostname it can do one thing
	// the fleet's cannot, which is lock the server down (lockdown.go).
	OperatorToken    string
	OperatorHostname string

	failMu   sync.Mutex
	failures map[string]*tokenBucket // per-source-IP auth-failure limiter

	// What bounds the memory files can take outside the hub's caps: how many
	// may be arriving, and how many leaving, at once (filesapi.go).
	uploads, downloads *gate

	// Lockdown (lockdown.go). LockPath is the file that keeps the server
	// locked over a restart; empty, a lock lasts as long as the process.
	LockPath string
	lock     lockState
}

type ctxKey int

const operatorKey ctxKey = 1

// isOperator reports a request that authenticated with the operator token.
func isOperator(r *http.Request) bool {
	v, _ := r.Context().Value(operatorKey).(bool)
	return v
}

// reserved reports a hostname only the operator may declare. Case is
// ignored: "Hub" beside "hub" in a room would fool exactly the reader this
// exists for.
//
// So would "һub" with a Cyrillic һ, or fullwidth "ｈｕｂ" — and there is no
// end of letters that look like other letters. A machine's hostname is
// ASCII, so on a server with an operator a hostname that is not is taken
// for an imitation of the operator's and kept for the operator too, rather
// than this trying to list them. (Without an operator nothing is reserved
// and any hostname is as good as another, as before.)
func (a *API) reserved(hostname string) bool {
	if a.OperatorToken == "" || a.OperatorHostname == "" {
		return false
	}
	if strings.EqualFold(hostname, a.OperatorHostname) {
		return true
	}
	for i := 0; i < len(hostname); i++ {
		if hostname[i] >= 0x80 {
			return true
		}
	}
	return false
}

// foreignOperatorPID reports a pid that belongs to the operator in a request
// that does not.
func (a *API) foreignOperatorPID(r *http.Request, room, pid string) bool {
	return a.OperatorToken != "" && !isOperator(r) && a.Hub.OperatorPID(room, pid)
}

// guardPID refuses a pid that belongs to the operator to anyone else. It
// writes the response itself and returns false when it refuses.
func (a *API) guardPID(w http.ResponseWriter, r *http.Request, room, pid string) bool {
	if !a.foreignOperatorPID(r, room, pid) {
		return true
	}
	log.Printf("auth: operator pid presented without the operator token from %s (%s %s)", sourceIP(r), r.Method, r.URL.Path)
	writeErr(w, http.StatusForbidden, proto.ErrForbidden, "this pid belongs to the operator")
	return false
}

func NewAPI(hub *Hub, token string) *API {
	return &API{
		Hub: hub, Token: token, failures: make(map[string]*tokenBucket),
		uploads:   newGate(maxUploads, maxUploadsPerSource),
		downloads: newGate(maxDownloads, maxDownloadsPerSrc),
	}
}

// Handler returns the full route table. Distribution endpoints (/install.sh,
// /bin/*) land here in Phase 3; /version is already served unauthenticated
// because `crosstalk update` and the install script need it before auth.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /version", a.handleVersion)
	mux.HandleFunc("GET /install.sh", handleInstallScript)
	mux.HandleFunc("GET /bin/{file}", handleBinary)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/rooms/{room}/join", a.handleJoin)
	api.HandleFunc("POST /api/rooms/{room}/messages", a.handleSend)
	api.HandleFunc("POST /api/rooms/{room}/files", a.handleSendFile)
	api.HandleFunc("GET /api/rooms/{room}/files/{id}", a.handleFile)
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
	api.HandleFunc("POST /api/lockdown", a.handleLockdown)

	mux.Handle("/api/", a.authenticate(api))
	// Outermost: a locked server answers nothing these routes would.
	return a.guard(mux)
}

func (a *API) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.VersionResponse{
		Version: proto.Version, Build: proto.Build, Proto: proto.ProtoVersion, MaxFile: a.Hub.MaxFile(),
		Locked: a.Locked(),
	})
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
		// Both compared, always: which one matched must not show in the timing.
		fleet := subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) == 1
		operator := a.OperatorToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.OperatorToken)) == 1
		if operator {
			r = r.WithContext(context.WithValue(r.Context(), operatorKey, true))
		}
		if !fleet && !operator {
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
	id := proto.Identity{Hostname: req.Hostname, Name: req.Name, Mode: req.Mode, Task: req.Task}
	var (
		resp proto.JoinResponse
		err  error
	)
	switch {
	case a.reserved(req.Hostname) && !isOperator(r):
		log.Printf("auth: join as reserved hostname %q refused from %s", req.Hostname, sourceIP(r))
		writeErr(w, http.StatusForbidden, proto.ErrForbidden,
			fmt.Sprintf("the hostname %q is reserved for the operator of this server (as is any hostname that is not plain ASCII)", req.Hostname))
		return
	case a.reserved(req.Hostname):
		resp, err = a.Hub.JoinAsOperator(r.PathValue("room"), id, sourceIP(r))
	default:
		resp, err = a.Hub.Join(r.PathValue("room"), id, sourceIP(r))
	}
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
	if !a.guardPID(w, r, r.PathValue("room"), req.PID) {
		return
	}
	ev, unresolved, err := a.Hub.Send(r.PathValue("room"), req.PID, req.Body, req.Kind)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SendResponse{ID: ev.ID, Mentions: ev.Mentions, Unresolved: unresolved})
}

func (a *API) handleTask(w http.ResponseWriter, r *http.Request) {
	var req proto.TaskRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !a.guardPID(w, r, r.PathValue("room"), req.PID) {
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
	if !a.guardPID(w, r, r.PathValue("room"), req.PID) {
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

	if !a.guardPID(w, r, room, pid) {
		return
	}
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
