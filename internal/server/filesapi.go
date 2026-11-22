package server

import (
	"context"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// How files travel, and what that may cost the server beyond the hub's caps.
//
// A file arrives as the body of one request, with its length said up front
// (Content-Length is required), and who is sending it in the query. So the
// server knows everything it needs to refuse before it has read a byte, and
// when it does read, it reads into exactly the bytes it was told to expect:
// what is held is what is counted, never a buffer that grew past it.
//
// Two things hold a file's bytes outside the caps, and each is bounded by a
// gate: an upload, from the moment its memory is made until the hub has
// counted it (maxUploads at once), and a download still being written when
// its file is evicted (maxDownloads at once). Neither may dawdle: a transfer
// gets the time it would need at fileFloorRate, and no more.
const (
	maxUploads          = 4
	maxUploadsPerSource = 2
	maxDownloads        = 8
	maxDownloadsPerSrc  = 4

	// fileFloorRate is the slowest a transfer may go, in bytes a second, and
	// fileGrace what it gets on top; fileTransferMax bounds the sum.
	fileFloorRate   = 64 * 1024
	fileGrace       = 30 * time.Second
	fileTransferMax = 10 * time.Minute
)

// slotWait is how long a transfer waits for its turn before it is told to
// come back. A variable for the tests' sake.
var slotWait = 30 * time.Second

// transferTime is how long a file of this size may take to arrive or leave.
// The server's ordinary write timeout is sized for JSON and a long poll and
// would cut a large file on a slow link; a flat long one would let a few
// trickling transfers hold every slot.
func transferTime(size int64) time.Duration {
	return min(fileGrace+time.Duration(size/fileFloorRate)*time.Second, fileTransferMax)
}

// gate admits a bounded number of transfers at once, and of those a bounded
// number from one address, so that one machine cannot hold every slot.
// Waiting for a slot holds nothing but the request.
type gate struct {
	max, perSource int

	mu      sync.Mutex
	total   int
	by      map[string]int
	changed chan struct{} // closed, and replaced, each time a slot is given back
}

func newGate(max, perSource int) *gate {
	return &gate{max: max, perSource: perSource, by: make(map[string]int), changed: make(chan struct{})}
}

// enter waits for a slot for up to wait. It returns the function that gives
// the slot back, or false when there was none in time (or the request ended).
// A nil gate admits everything.
func (g *gate) enter(ctx context.Context, source string, wait time.Duration) (leave func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		g.mu.Lock()
		if g.total < g.max && g.by[source] < g.perSource {
			g.total++
			g.by[source]++
			g.mu.Unlock()
			return func() { g.leave(source) }, true
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (g *gate) leave(source string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.total--
	if g.by[source]--; g.by[source] <= 0 {
		delete(g.by, source)
	}
	close(g.changed)
	g.changed = make(chan struct{})
}

func errBusy() *apiError {
	return &apiError{429, proto.ErrRateLimited, "the server is busy with other files: try again shortly", 5 * time.Second}
}

// handleSendFile takes a message with a file:
//
//	POST /api/rooms/{room}/files?pid=<pid>&name=<file name>&body=<caption>
//
// with the file's bytes as the request body. See proto.File.
func (a *API) handleSendFile(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	q := r.URL.Query()
	pid, name, caption := q.Get("pid"), q.Get("name"), q.Get("body")
	size := r.ContentLength

	// Everything that can refuse this file is asked now, before any of it is
	// read: is it a size the server takes, may this pid speak, is it being
	// paced.
	var refusal error
	switch {
	case a.Hub.MaxFile() == 0:
		refusal = errNoFiles()
	case size < 0:
		refusal = &apiError{http.StatusLengthRequired, proto.ErrBadRequest, "a file is sent with its length: Content-Length is required", 0}
	case a.foreignOperatorPID(r, room, pid):
		log.Printf("auth: operator pid presented without the operator token from %s (%s %s)", sourceIP(r), r.Method, r.URL.Path)
		refusal = &apiError{http.StatusForbidden, proto.ErrForbidden, "this pid belongs to the operator", 0}
	default:
		refusal = a.Hub.CheckFile(room, pid, caption, size)
	}
	if refusal != nil {
		a.refuseFile(w, r, refusal)
		return
	}
	leave, ok := a.uploads.enter(r.Context(), sourceIP(r), slotWait)
	if !ok {
		a.refuseFile(w, r, errBusy())
		return
	}
	defer leave()

	rc := http.NewResponseController(w)
	allowed := transferTime(size)
	_ = rc.SetReadDeadline(time.Now().Add(allowed))
	_ = rc.SetWriteDeadline(time.Now().Add(allowed + time.Minute))
	data := make([]byte, size)
	if _, err := io.ReadFull(r.Body, data); err != nil {
		// Less came than was announced, or it came too slowly: there is no
		// file, and nothing more to read on this connection.
		w.Header().Set("Connection", "close")
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "the file did not arrive whole: "+err.Error())
		return
	}

	ev, unresolved, err := a.Hub.SendFile(room, pid, caption, name, data)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proto.SendResponse{ID: ev.ID, Mentions: ev.Mentions, Unresolved: unresolved, File: ev.File})
}

// refuseFile answers an upload that will not be taken, in a way the client
// can hear. A client that asked first (Expect: 100-continue, as crosstalk's
// own do) has sent nothing yet and is simply answered. One that is already
// sending cannot hear an answer over HTTP/1.1 until it has finished — it
// would see a broken connection, not the reason — so what it is sending is
// read and thrown away first, as long as that is no more than a file may be;
// past that the connection is closed under it.
func (a *API) refuseFile(w http.ResponseWriter, r *http.Request, err error) {
	asked := strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue")
	if limit := a.Hub.MaxFile(); !asked && r.ContentLength >= 0 && r.ContentLength <= limit {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(transferTime(r.ContentLength)))
		_, _ = io.Copy(io.Discard, r.Body)
	} else {
		w.Header().Set("Connection", "close")
	}
	writeAPIErr(w, err)
}

// handleFile serves a file's bytes. Always as a download of unnamed type:
// what the bytes are is in the message's description of the file (and in
// X-Crosstalk-File-Type here), for a client to act on by its own rules —
// nothing that fetches from this server should render what a room sent just
// because a header told it to.
func (a *API) handleFile(w http.ResponseWriter, r *http.Request) {
	// The slot first, the bytes second: a request that is only waiting must
	// not be what keeps an evicted file in memory.
	leave, ok := a.downloads.enter(r.Context(), sourceIP(r), slotWait)
	if !ok {
		writeAPIErr(w, errBusy())
		return
	}
	defer leave()

	meta, data, err := a.Hub.File(r.PathValue("room"), r.PathValue("id"))
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Crosstalk-File-Type", meta.Type)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": meta.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", disposition)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferTime(int64(len(data)))))
	_, _ = w.Write(data)
}
