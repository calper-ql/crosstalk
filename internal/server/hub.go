// Package server implements crosstalkd: an in-memory hub with per-room
// event logs and a set of parked long-poll requests to complete when events
// arrive. The server process is the single source of truth; a restart loses
// all conversation state by design.
package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Config carries the tunables from §4 of the design doc. Zero values are
// replaced by defaults in NewHub.
type Config struct {
	RoomCap    int           // max events kept per room (oldest evicted)
	IdleExpiry time.Duration // delete rooms with no messages for this long
	GCInterval time.Duration // sweep cadence
	RatePerMin float64       // sustained messages/minute per pid
	Burst      float64       // token-bucket burst per pid
	MaxBody    int           // max message body bytes
	MaxWait    time.Duration // cap on the events long-poll hold
}

func (c *Config) fillDefaults() {
	if c.RoomCap == 0 {
		c.RoomCap = 10000
	}
	if c.IdleExpiry == 0 {
		c.IdleExpiry = 8 * time.Hour
	}
	if c.GCInterval == 0 {
		c.GCInterval = 2 * time.Minute
	}
	if c.RatePerMin == 0 {
		c.RatePerMin = 30
	}
	if c.Burst == 0 {
		c.Burst = 5
	}
	if c.MaxBody == 0 {
		c.MaxBody = 16 * 1024
	}
	if c.MaxWait == 0 {
		c.MaxWait = 30 * time.Second
	}
}

// apiError is returned by hub operations and mapped to HTTP by the API layer.
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errPIDGone() *apiError {
	return &apiError{410, proto.ErrPIDGone, "pid not known to this server (room expired, left, or server restarted): rejoin and redeclare your task", 0}
}

type participant struct {
	pid      string
	identity proto.Identity
	joined   time.Time
	lastSeen time.Time
	left     bool
	remote   string
	bucket   tokenBucket
}

type room struct {
	name         string
	created      time.Time
	lastMessage  time.Time // drives idle expiry
	lastActivity time.Time // any event; drives TUI sort order
	events       []proto.Event
	evicted      bool // true once anything has fallen off the ring
	participants map[string]*participant
	waiters      map[string]chan struct{} // parked polls, one per pid
	closed       bool                     // tombstone: GC'd, deleted next sweep
}

// Hub is the whole server state. All maps are guarded by mu; long-poll
// waiting happens outside the lock on per-pid channels.
type Hub struct {
	mu    sync.Mutex
	cfg   Config
	rooms map[string]*room
	ulid  *proto.ULIDGen
	now   func() time.Time // test seam
}

func NewHub(cfg Config) *Hub {
	cfg.fillDefaults()
	return &Hub{
		cfg:   cfg,
		rooms: make(map[string]*room),
		ulid:  proto.NewULIDGen(),
		now:   time.Now,
	}
}

// getRoom returns the live room, creating it if absent or tombstoned.
func (h *Hub) getRoom(name string) *room {
	r := h.rooms[name]
	if r == nil || r.closed {
		r = &room{
			name:         name,
			created:      h.now(),
			lastMessage:  h.now(),
			lastActivity: h.now(),
			participants: make(map[string]*participant),
			waiters:      make(map[string]chan struct{}),
		}
		h.rooms[name] = r
	}
	return r
}

// livePID resolves a pid to its participant in a live room, or nil.
func (h *Hub) livePID(roomName, pid string) (*room, *participant) {
	r := h.rooms[roomName]
	if r == nil || r.closed {
		return nil, nil
	}
	p := r.participants[pid]
	if p == nil || p.left {
		return nil, nil
	}
	return r, p
}

// append adds an event to the room's log, evicting past the cap, and wakes
// every parked poll. Callers hold h.mu.
func (h *Hub) append(r *room, ev proto.Event) proto.Event {
	ev.ID = h.ulid.New(h.now())
	ev.TS = h.now().UTC().Truncate(time.Millisecond)
	r.events = append(r.events, ev)
	if len(r.events) > h.cfg.RoomCap {
		over := len(r.events) - h.cfg.RoomCap
		r.events = append([]proto.Event(nil), r.events[over:]...)
		r.evicted = true
	}
	r.lastActivity = h.now()
	if ev.Type == proto.TypeMessage {
		r.lastMessage = h.now()
	}
	for pid, ch := range r.waiters {
		close(ch)
		delete(r.waiters, pid)
	}
	return ev
}

// Join creates the room if needed, mints a pid bound to the declared
// identity, and returns the pid plus the room's current head cursor.
func (h *Hub) Join(roomName string, id proto.Identity, remote string) (proto.JoinResponse, error) {
	if err := validateIdentity(id); err != nil {
		return proto.JoinResponse{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.getRoom(roomName)
	p := &participant{
		pid:      proto.NewPID(),
		identity: id,
		joined:   h.now(),
		lastSeen: h.now(),
		remote:   remote,
	}
	r.participants[p.pid] = p
	h.append(r, proto.Event{Type: proto.TypePresence, Event: proto.PresenceJoin, Who: &p.identity})

	return proto.JoinResponse{PID: p.pid, Cursor: r.events[len(r.events)-1].ID}, nil
}

// Send posts a message. The From identity is resolved from the pid — the
// sender has no say in it — and mentions are parsed and resolved server-side.
func (h *Hub) Send(roomName, pid, body, kind string) (proto.Event, error) {
	if kind == "" {
		kind = proto.KindText
	}
	if kind != proto.KindText && kind != proto.KindStatus && kind != proto.KindCode {
		return proto.Event{}, &apiError{400, proto.ErrBadRequest, "kind must be text, status, or code", 0}
	}
	if len(body) > h.cfg.MaxBody {
		return proto.Event{}, &apiError{413, proto.ErrTooLarge,
			fmt.Sprintf("message body is %d bytes; the cap is %d — put large content in a file or the issue and reference it by path", len(body), h.cfg.MaxBody), 0}
	}
	if strings.TrimSpace(body) == "" {
		return proto.Event{}, &apiError{400, proto.ErrBadRequest, "empty message body", 0}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		return proto.Event{}, errPIDGone()
	}
	if wait, ok := p.bucket.take(h.now(), h.cfg.RatePerMin/60.0, h.cfg.Burst); !ok {
		return proto.Event{}, &apiError{429, proto.ErrRateLimited,
			"rate limited: keep the conversation followable by a human", wait}
	}
	p.lastSeen = h.now()

	id := p.identity // snapshot, includes current task
	ev := h.append(r, proto.Event{
		Type:     proto.TypeMessage,
		From:     &id,
		Body:     body,
		Kind:     kind,
		Mentions: resolveMentions(body, r),
	})
	return ev, nil
}

// SetTask updates the participant's current-activity line and broadcasts it
// as a presence frame.
func (h *Hub) SetTask(roomName, pid, task string) error {
	if len(task) > 512 {
		return &apiError{400, proto.ErrBadRequest, "task description too long (max 512 bytes)", 0}
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		return errPIDGone()
	}
	p.lastSeen = h.now()
	p.identity.Task = task
	id := p.identity
	h.append(r, proto.Event{Type: proto.TypePresence, Event: proto.PresenceTask, Who: &id})
	return nil
}

// Leave retires the pid. The participant stays in room history for mention
// resolution, but the pid can no longer write.
func (h *Hub) Leave(roomName, pid string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		return errPIDGone()
	}
	p.left = true
	p.lastSeen = h.now()
	if ch, ok := r.waiters[pid]; ok {
		close(ch)
		delete(r.waiters, pid)
	}
	id := p.identity
	h.append(r, proto.Event{Type: proto.TypePresence, Event: proto.PresenceLeave, Who: &id})
	return nil
}

// EventsAfter returns all buffered events newer than the cursor, without
// blocking. A cursor that has fallen off the ring is a distinct error from a
// dead pid: the pid is fine, but the gap cannot be verified — the caller
// resumes from the present.
func (h *Hub) EventsAfter(roomName, pid, after string) ([]proto.Event, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		// A tombstoned room still answers its parked participants' next
		// poll with the room_closed event before the sweep deletes it.
		if tr := h.rooms[roomName]; tr != nil && tr.closed && tr.participants[pid] != nil {
			return []proto.Event{{
				Type:   proto.TypeRoomClosed,
				ID:     h.ulid.New(h.now()),
				TS:     h.now().UTC(),
				Reason: "idle_expiry",
			}}, nil
		}
		return nil, errPIDGone()
	}
	p.lastSeen = h.now()

	if after != "" && r.evicted && (len(r.events) == 0 || after < r.events[0].ID) {
		return nil, &apiError{410, proto.ErrCursorGone,
			"cursor no longer in the room buffer: resume from the present via a fresh join cursor or the history endpoint", 0}
	}
	i := sort.Search(len(r.events), func(i int) bool { return r.events[i].ID > after })
	if i >= len(r.events) {
		return nil, nil
	}
	out := make([]proto.Event, len(r.events)-i)
	copy(out, r.events[i:])
	return out, nil
}

// Park registers a long-poll waiter for the pid, superseding any existing
// one — the superseded poll wakes, finds nothing new, and returns empty,
// which doubles as self-healing for orphaned wait processes.
func (h *Hub) Park(roomName, pid string) (<-chan struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, p := h.livePID(roomName, pid)
	if p == nil {
		return nil, errPIDGone()
	}
	if old, ok := r.waiters[pid]; ok {
		close(old)
	}
	ch := make(chan struct{})
	r.waiters[pid] = ch
	return ch, nil
}

// Unpark removes the waiter if it is still the registered one (a timed-out
// or cancelled poll cleaning up after itself).
func (h *Hub) Unpark(roomName, pid string, ch <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[roomName]
	if r == nil {
		return
	}
	if cur, ok := r.waiters[pid]; ok && cur == ch {
		close(cur)
		delete(r.waiters, pid)
	}
}

// History returns up to limit events strictly older than before (or the
// newest events when before is empty), in chronological order.
func (h *Hub) History(roomName, before string, limit int) ([]proto.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[roomName]
	if r == nil || r.closed {
		return nil, &apiError{404, proto.ErrNotFound, "no such room", 0}
	}
	end := len(r.events)
	if before != "" {
		end = sort.Search(len(r.events), func(i int) bool { return r.events[i].ID >= before })
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := make([]proto.Event, end-start)
	copy(out, r.events[start:end])
	return out, nil
}

// Rooms lists live rooms sorted by last activity, newest first.
func (h *Hub) Rooms() []proto.RoomInfo {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]proto.RoomInfo, 0, len(h.rooms))
	for _, r := range h.rooms {
		if r.closed {
			continue
		}
		active := 0
		for _, p := range r.participants {
			if !p.left {
				active++
			}
		}
		out = append(out, proto.RoomInfo{
			Name:         r.name,
			Created:      r.created.UTC(),
			LastActivity: r.lastActivity.UTC(),
			Participants: active,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivity.After(out[j].LastActivity) })
	return out
}

// Participants lists a room's current (non-departed) participants, newest
// join last.
func (h *Hub) Participants(roomName string) ([]proto.ParticipantInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[roomName]
	if r == nil || r.closed {
		return nil, &apiError{404, proto.ErrNotFound, "no such room", 0}
	}
	out := make([]proto.ParticipantInfo, 0, len(r.participants))
	for _, p := range r.participants {
		if p.left {
			continue
		}
		out = append(out, proto.ParticipantInfo{
			Identity: p.identity,
			Joined:   p.joined.UTC(),
			LastSeen: p.lastSeen.UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Joined.Before(out[j].Joined) })
	return out, nil
}

// Sweep deletes tombstoned rooms and tombstones rooms idle past the expiry,
// waking their parked polls so live listeners see room_closed on their next
// poll. It returns the names of rooms expired this pass.
func (h *Hub) Sweep() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var expired []string
	for name, r := range h.rooms {
		if r.closed {
			delete(h.rooms, name)
			continue
		}
		if h.now().Sub(r.lastMessage) >= h.cfg.IdleExpiry {
			r.closed = true
			expired = append(expired, name)
			for pid, ch := range r.waiters {
				close(ch)
				delete(r.waiters, pid)
			}
		}
	}
	return expired
}

// RunGC sweeps on the configured interval until stop is closed.
func (h *Hub) RunGC(stop <-chan struct{}, logf func(format string, v ...any)) {
	t := time.NewTicker(h.cfg.GCInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if expired := h.Sweep(); len(expired) > 0 && logf != nil {
				logf("gc: expired idle rooms: %s", strings.Join(expired, ", "))
			}
		}
	}
}

func (h *Hub) MaxWait() time.Duration { return h.cfg.MaxWait }

func validateIdentity(id proto.Identity) error {
	bad := func(msg string) error { return &apiError{400, proto.ErrBadRequest, msg, 0} }
	if id.Mode != proto.ModeAgent && id.Mode != proto.ModeHuman {
		return bad("mode must be agent or human")
	}
	if !nameRe.MatchString(id.Name) {
		return bad("name must be 1-64 chars of letters, digits, . _ -")
	}
	if !nameRe.MatchString(id.Hostname) {
		return bad("hostname must be 1-64 chars of letters, digits, . _ -")
	}
	if id.Mode == proto.ModeAgent && strings.TrimSpace(id.Task) == "" {
		return bad("task is required for agents: one line describing what you are doing")
	}
	if len(id.Task) > 512 {
		return bad("task description too long (max 512 bytes)")
	}
	return nil
}

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}._-]{1,64}$`)

// mentionRe matches @hostname/name, @name, and @room in message bodies.
var mentionRe = regexp.MustCompile(`@([\p{L}\p{N}._-]+)(?:/([\p{L}\p{N}._-]+))?`)

// resolveMentions parses @-references and resolves them against the room's
// participant history (including departed participants). Text that resolves
// to nobody stays plain text. Caller holds h.mu.
func resolveMentions(body string, r *room) []proto.Mention {
	var out []proto.Mention
	seen := make(map[string]bool)
	add := func(m proto.Mention) {
		key := m.Hostname + "\x00" + m.Name
		if m.All {
			key = "\x00all"
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, m)
		}
	}
	// The token class includes . _ - so "@worker-3." (mention followed by
	// sentence punctuation) initially captures the period. resolveTrimming
	// retries with trailing punctuation stripped until something resolves:
	// resolution against real participants is the arbiter, not the regex.
	resolveTrimming := func(s string, ok func(string) bool) (string, bool) {
		for s != "" {
			if ok(s) {
				return s, true
			}
			switch s[len(s)-1] {
			case '.', '_', '-':
				s = s[:len(s)-1]
			default:
				return "", false
			}
		}
		return "", false
	}

	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		first, second := m[1], m[2]
		if second == "" {
			if _, ok := resolveTrimming(first, func(s string) bool { return s == "room" }); ok {
				add(proto.Mention{All: true})
				continue
			}
			// Bare @name: resolve to every participant with that name,
			// on any host.
			name, ok := resolveTrimming(first, func(s string) bool {
				for _, p := range r.participants {
					if p.identity.Name == s {
						return true
					}
				}
				return false
			})
			if !ok {
				continue
			}
			for _, p := range r.participants {
				if p.identity.Name == name {
					add(proto.Mention{Hostname: p.identity.Hostname, Name: p.identity.Name})
				}
			}
			continue
		}
		// @hostname/name: exact match against participant history.
		name, ok := resolveTrimming(second, func(s string) bool {
			for _, p := range r.participants {
				if p.identity.Hostname == first && p.identity.Name == s {
					return true
				}
			}
			return false
		})
		if ok {
			add(proto.Mention{Hostname: first, Name: name})
		}
	}
	return out
}

// tokenBucket is the per-pid flow-control bucket: capacity Burst, refill
// RatePerMin. It exists to keep conversations followable by a human, not to
// protect the server.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// take attempts to spend one token; on refusal it reports how long until a
// token is available.
func (b *tokenBucket) take(now time.Time, ratePerSec, burst float64) (time.Duration, bool) {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * ratePerSec
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / ratePerSec * float64(time.Second)), false
}
