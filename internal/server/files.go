package server

import (
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Files. A message can carry one file — a screenshot, a log, a patch — and
// the file is kept exactly as the message is: in this process's memory, in
// its room, and nowhere else. Nothing is written to disk. So a file lives no
// longer than its room (the idle sweep takes both), no longer than its
// message (the per-room event cap takes both), and a restart loses it with
// everything else.
//
// Memory is the one thing a file can use up that a line of text cannot, so
// three caps bound it, and none of them ever refuses a file that is itself
// within the first:
//
//   - MaxFile: one file. Larger is refused.
//   - RoomFiles: one room's files together. Past it the room's oldest files
//     go, the same rule the event cap applies to messages.
//   - TotalFiles: every room's files together. Past it the oldest file on
//     the server goes, whichever room holds it.
//
// A message whose file has gone stays in the history, description and all;
// fetching the bytes then answers not_found.

// storedFile is one file in memory. Neither field changes once it is stored,
// so the bytes can be written to a response without holding the hub's lock.
type storedFile struct {
	meta proto.File
	data []byte
}

// maxFileName bounds a file's name, in bytes. Long enough for any real name,
// short enough to sit in a header and on one line.
const maxFileName = 200

func errNoFiles() *apiError {
	return &apiError{404, proto.ErrNotFound, "this server takes no files", 0}
}

func errNoFile() *apiError {
	return &apiError{404, proto.ErrNotFound,
		"no such file in this room: a file is kept in memory only — it goes when its room does, and earlier when newer files need the space", 0}
}

// MaxFile is the largest file a message may carry, in bytes; zero when this
// server takes none.
func (h *Hub) MaxFile() int64 {
	if h.cfg.MaxFile <= 0 {
		return 0
	}
	return int64(h.cfg.MaxFile)
}

// CheckFile reports, without spending anything, whether the pid could send
// a file of this size with this caption right now: the server takes files,
// the size and the caption are within their caps, the pid is live, and it is
// not being paced. The API asks before it reads a byte of the file, so a
// sender that will be refused is refused before the server has made room for
// it. SendFile checks everything again; this is only the early answer.
func (h *Hub) CheckFile(roomName, pid, caption string, size int64) error {
	switch {
	case h.cfg.MaxFile <= 0:
		return errNoFiles()
	case size <= 0:
		return &apiError{400, proto.ErrBadRequest, "empty file", 0}
	case size > int64(h.cfg.MaxFile):
		return errFileTooLarge(size, int64(h.cfg.MaxFile))
	case len(caption) > h.cfg.MaxBody:
		return errCaptionTooLarge(len(caption), h.cfg.MaxBody)
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	_, p := h.livePID(roomName, pid)
	if p == nil {
		return errPIDGone()
	}
	if wait, ok := p.bucket.peek(h.now(), h.cfg.RatePerMin/60.0, h.cfg.Burst); !ok {
		return &apiError{429, proto.ErrRateLimited,
			"rate limited: keep the conversation followable by a human", wait}
	}
	return nil
}

func errCaptionTooLarge(size, limit int) *apiError {
	return &apiError{413, proto.ErrTooLarge, fmt.Sprintf("the caption is %d bytes; the cap is %d", size, limit), 0}
}

// SendFile posts a message that carries a file. body is its caption and may
// be empty; mentions in it resolve as in any message. The name is cleaned
// (cleanFileName) and the type is read from the bytes, so neither is the
// sender's to forge. data is kept as given: the caller must not touch it
// afterwards.
func (h *Hub) SendFile(roomName, pid, body, name string, data []byte) (proto.Event, []string, error) {
	switch {
	case h.cfg.MaxFile <= 0:
		return proto.Event{}, nil, errNoFiles()
	case len(data) == 0:
		return proto.Event{}, nil, &apiError{400, proto.ErrBadRequest, "empty file", 0}
	case len(data) > h.cfg.MaxFile:
		return proto.Event{}, nil, errFileTooLarge(int64(len(data)), int64(h.cfg.MaxFile))
	case len(body) > h.cfg.MaxBody:
		return proto.Event{}, nil, errCaptionTooLarge(len(body), h.cfg.MaxBody)
	}
	// What is kept is what is counted. A slice with room to spare would hold
	// that room for as long as the file lives, where no cap can see it: it is
	// copied down to its length first.
	if cap(data) != len(data) {
		data = append(make([]byte, 0, len(data)), data...)
	}
	size := int64(len(data))
	// Before the lock: a name is the sender's to make as long as it likes.
	name = cleanFileName(name)
	kind := http.DetectContentType(data)

	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		return proto.Event{}, nil, errPIDGone()
	}
	if wait, ok := p.bucket.take(h.now(), h.cfg.RatePerMin/60.0, h.cfg.Burst); !ok {
		return proto.Event{}, nil, &apiError{429, proto.ErrRateLimited,
			"rate limited: keep the conversation followable by a human", wait}
	}
	p.lastSeen = h.now()

	h.makeSpace(r, size)
	meta := proto.File{ID: h.ulid.New(h.now()), Name: name, Size: size, Type: kind}
	if r.files == nil {
		r.files = make(map[string]*storedFile)
	}
	r.files[meta.ID] = &storedFile{meta: meta, data: data}
	r.fileOrder = append(r.fileOrder, meta.ID)
	r.fileBytes += size
	h.fileBytes += size

	mentions, unresolved := resolveMentions(body, r)
	id := p.identity // snapshot, includes current task
	ev := h.append(r, proto.Event{
		Type:     proto.TypeMessage,
		From:     &id,
		Body:     body,
		Kind:     proto.KindText,
		Mentions: mentions,
		File:     &meta,
	})
	return ev, unresolved, nil
}

// errFileTooLarge names the cap, and the file's size when it is known (zero:
// the server stopped reading before it found out).
func errFileTooLarge(size, limit int64) *apiError {
	msg := fmt.Sprintf("the file is too large: this server takes files up to %d bytes", limit)
	if size > 0 {
		msg = fmt.Sprintf("the file is %d bytes; this server takes files up to %d", size, limit)
	}
	return &apiError{413, proto.ErrTooLarge, msg, 0}
}

// File returns a file's description and its bytes. Like the history, it asks
// for no pid: whoever holds the token can read any room.
func (h *Hub) File(roomName, id string) (proto.File, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[roomName]
	if r == nil || r.closed {
		return proto.File{}, nil, errNoRoom()
	}
	f := r.files[id]
	if f == nil {
		return proto.File{}, nil, errNoFile()
	}
	return f.meta, f.data, nil
}

// FileBytes reports what the server's files add up to. For tests and logs.
func (h *Hub) FileBytes() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fileBytes
}

// makeSpace evicts until a file of size bytes fits: first within the room,
// then on the server as a whole. Callers hold h.mu.
func (h *Hub) makeSpace(r *room, size int64) {
	for r.fileBytes+size > h.cfg.RoomFiles && len(r.fileOrder) > 0 {
		h.dropFile(r, r.fileOrder[0])
	}
	for h.fileBytes+size > h.cfg.TotalFiles {
		// The oldest file anywhere. Ids are ULIDs, so they order by time, and
		// each room's first is its oldest.
		var oldest *room
		for _, x := range h.rooms {
			if len(x.fileOrder) > 0 && (oldest == nil || x.fileOrder[0] < oldest.fileOrder[0]) {
				oldest = x
			}
		}
		if oldest == nil {
			return
		}
		h.dropFile(oldest, oldest.fileOrder[0])
	}
}

// dropFile forgets one file of a room. A file already gone is no error: the
// caps may have taken it before its message was evicted. Callers hold h.mu.
func (h *Hub) dropFile(r *room, id string) {
	f := r.files[id]
	if f == nil {
		return
	}
	delete(r.files, id)
	for i, x := range r.fileOrder { // evictions come from the front: this is short
		if x == id {
			r.fileOrder = append(r.fileOrder[:i], r.fileOrder[i+1:]...)
			break
		}
	}
	r.fileBytes -= f.meta.Size
	h.fileBytes -= f.meta.Size
}

// dropFiles forgets every file of a room. Callers hold h.mu.
func (h *Hub) dropFiles(r *room) {
	h.fileBytes -= r.fileBytes
	r.files, r.fileOrder, r.fileBytes = nil, nil, 0
}

// cleanFileName makes a sender's name for a file fit to show and to save
// under: the last path element only (by either separator — the sender may be
// on any system), no control or formatting characters (a newline would forge
// a line in a terminal, a right-to-left override would disguise an
// extension), valid UTF-8, bounded. A name with nothing left is "file".
func cleanFileName(name string) string {
	// The name proper is at the end, after the last separator; nothing further
	// back than this could be part of what is kept.
	if len(name) > 8*maxFileName {
		name = name[len(name)-8*maxFileName:]
	}
	name = strings.ToValidUTF8(name, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > maxFileName {
		// Keep the extension: it is what says how to open the thing.
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:maxFileName-len(ext)]
		for !utf8.ValidString(stem) { // cut inside a character: back off to its start
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimSpace(stem) + ext
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// peek is take without spending: whether a token is there now, and if not,
// how long until one is.
func (b *tokenBucket) peek(now time.Time, ratePerSec, burst float64) (time.Duration, bool) {
	c := *b
	return c.take(now, ratePerSec, burst)
}
