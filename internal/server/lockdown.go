package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Lockdown: the operator's way to take the whole server out of service at
// once, when something is wrong and nothing should be said to anyone.
//
// A locked server answers /version (which says it is locked) and nothing
// else. Every room, message and file it held is dropped, every request in
// flight is cut, and every later one is refused with locked_down.
//
// No request lifts it. The lock is a file, "lockdown", beside the
// certificate: while it is there the server starts locked, so a crash, a
// restart or a reboot does not lift it either. Removing that file and
// restarting the server is the only way back — which is to say, a person at
// the machine.
//
// It is asked for with the operator token (POST /api/lockdown), or by putting
// the file there and restarting. The fleet token cannot ask: it is on every
// machine, and any one of them could then take the server from all the rest.

// LockFile is the name of the lock, in the state directory.
const LockFile = "lockdown"

func errLocked() *apiError {
	return &apiError{http.StatusServiceUnavailable, proto.ErrLockedDown, "this server is locked down", 0}
}

type lockState struct {
	on atomic.Bool

	// Every request being served, so that a lock can cut them. mu is held to
	// add one, to remove one, and for the whole of the cut: a request is
	// either refused at the door or in this set when the cut is made.
	mu      sync.Mutex
	flights map[*flight]struct{}
}

// flight is one request being served.
type flight struct {
	rc     *http.ResponseController
	cancel context.CancelFunc
}

type flightKey struct{}

// Locked reports a server that is locked down.
func (a *API) Locked() bool { return a.lock.on.Load() }

// LoadLock locks the server if the lock file is there. Called once, before
// the listener starts. A file that cannot be looked at is taken for a lock:
// the question is whether it is safe to serve, and "cannot tell" is a no.
func (a *API) LoadLock() bool {
	if a.LockPath == "" {
		return false
	}
	if _, err := os.Stat(a.LockPath); err != nil && errors.Is(err, os.ErrNotExist) {
		return false
	}
	a.lock.on.Store(true)
	a.Hub.Clear()
	return true
}

// Lock locks the server down, now. It reports whether the lock was written
// where the next start will find it; the server is locked either way.
//
// The order is the point: first nothing new is let in, then the lock is put
// on disk, then what the server holds is dropped, then whatever was already
// being served is cut — all but the request that asked, if it was one (ctx),
// which still has an answer to write.
func (a *API) Lock(ctx context.Context, why string) (kept bool, err error) {
	a.lock.on.Store(true)
	if a.LockPath != "" {
		err = writeLock(a.LockPath, why)
		kept = err == nil
	}
	rooms := a.Hub.Clear()

	asker, _ := ctx.Value(flightKey{}).(*flight)
	past := time.Unix(1, 0)
	cut := 0
	a.lock.mu.Lock()
	for f := range a.lock.flights {
		if f == asker {
			continue
		}
		f.cancel()
		// Whatever it was reading or writing fails now, and the connection
		// goes with it. An error only says the connection is already gone.
		_ = f.rc.SetReadDeadline(past)
		_ = f.rc.SetWriteDeadline(past)
		cut++
	}
	a.lock.mu.Unlock()

	switch {
	case err != nil:
		log.Printf("LOCKED DOWN (%s): %d rooms dropped, %d requests cut. THE LOCK COULD NOT BE WRITTEN (%v): a restart will lift it", why, rooms, cut, err)
	case !kept:
		log.Printf("LOCKED DOWN (%s): %d rooms dropped, %d requests cut. No lock file is configured: a restart will lift it", why, rooms, cut)
	default:
		log.Printf("LOCKED DOWN (%s): %d rooms dropped, %d requests cut. To serve again, remove %s and restart", why, rooms, cut, a.LockPath)
	}
	return kept, err
}

// writeLock puts the lock file on disk, and waits until it is.
func writeLock(path, why string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "locked down %s: %s\nRemove this file and restart crosstalkd to serve again.\n",
		time.Now().UTC().Format(time.RFC3339), why)
	serr := f.Sync()
	cerr := f.Close()
	return errors.Join(werr, serr, cerr)
}

// guard is the door every request comes through. Locked, the server says so
// and nothing more; otherwise the request is served, and listed for as long
// as it is so that a lock can cut it.
func (a *API) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		f := &flight{rc: http.NewResponseController(w), cancel: cancel}

		a.lock.mu.Lock()
		locked := a.lock.on.Load() // read under mu: see lockState
		if !locked {
			if a.lock.flights == nil {
				a.lock.flights = make(map[*flight]struct{})
			}
			a.lock.flights[f] = struct{}{}
		}
		a.lock.mu.Unlock()
		if locked {
			if r.Method == http.MethodGet && r.URL.Path == "/version" {
				a.handleVersion(w, r)
				return
			}
			writeAPIErr(w, errLocked())
			return
		}
		defer func() {
			a.lock.mu.Lock()
			delete(a.lock.flights, f)
			a.lock.mu.Unlock()
		}()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, flightKey{}, f)))
	})
}

// handleLockdown locks the server down for the operator.
func (a *API) handleLockdown(w http.ResponseWriter, r *http.Request) {
	if !isOperator(r) {
		log.Printf("auth: lockdown asked for without the operator token from %s", sourceIP(r))
		writeErr(w, http.StatusForbidden, proto.ErrForbidden, "only the operator of this server can lock it down")
		return
	}
	kept, _ := a.Lock(r.Context(), "asked for by the operator, from "+sourceIP(r))
	writeJSON(w, http.StatusOK, proto.LockdownResponse{Locked: true, Kept: kept})
}

// Clear drops every room, with its messages, its files and its parked polls,
// and lets no room be made again. It returns how many rooms there were.
func (h *Hub) Clear() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.locked = true
	n := 0
	for name, r := range h.rooms {
		if !r.closed {
			n++
		}
		r.closed = true
		h.dropFiles(r)
		for pid, ch := range r.waiters {
			close(ch)
			delete(r.waiters, pid)
		}
		delete(h.rooms, name)
	}
	return n
}
