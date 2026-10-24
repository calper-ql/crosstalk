package server

import (
	"errors"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

func agentID(host, name string) proto.Identity {
	return proto.Identity{Hostname: host, Name: name, Mode: proto.ModeAgent, Task: "testing"}
}

// testClock lets tests advance time manually.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestHub(cfg Config) (*Hub, *testClock) {
	h := NewHub(cfg)
	c := &testClock{t: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	h.now = c.now
	return h, c
}

func mustJoin(t *testing.T, h *Hub, room string, id proto.Identity) proto.JoinResponse {
	t.Helper()
	resp, err := h.Join(room, id, "127.0.0.1")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return resp
}

func TestJoinSendEventsCursor(t *testing.T) {
	h, _ := newTestHub(Config{})
	w := mustJoin(t, h, "ai#42", agentID("zeus", "worker-3"))

	// The join cursor is the head: nothing new yet.
	evs, err := h.EventsAfter("ai#42", w.PID, w.Cursor)
	if err != nil || len(evs) != 0 {
		t.Fatalf("expected no events after join cursor, got %d, err %v", len(evs), err)
	}

	o := mustJoin(t, h, "ai#42", agentID("anubis", "oracle"))
	if _, _, err := h.Send("ai#42", o.PID, "hello @zeus/worker-3", ""); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Worker sees oracle's join presence and the message.
	evs, err = h.EventsAfter("ai#42", w.PID, w.Cursor)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs) != 2 || evs[0].Type != proto.TypePresence || evs[1].Type != proto.TypeMessage {
		t.Fatalf("unexpected events: %+v", evs)
	}
	msg := evs[1]
	if msg.From.Hostname != "anubis" || msg.From.Name != "oracle" {
		t.Fatalf("identity not resolved from pid: %+v", msg.From)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0].Hostname != "zeus" || msg.Mentions[0].Name != "worker-3" {
		t.Fatalf("mention not resolved: %+v", msg.Mentions)
	}

	// Cursor advances: nothing new after the message.
	evs, _ = h.EventsAfter("ai#42", w.PID, msg.ID)
	if len(evs) != 0 {
		t.Fatalf("expected nothing after head, got %d", len(evs))
	}
}

func TestMentionVariants(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "worker-3"))
	mustJoin(t, h, "r", agentID("anubis", "worker-3")) // same name, other host
	mustJoin(t, h, "r", agentID("anubis", "oracle"))

	ev, _, err := h.Send("r", a.PID, "ping @worker-3 and @room and @nobody-here", "")
	if err != nil {
		t.Fatal(err)
	}
	var all, w3 int
	for _, m := range ev.Mentions {
		if m.All {
			all++
		}
		if m.Name == "worker-3" {
			w3++
		}
	}
	if all != 1 {
		t.Fatalf("@room not resolved: %+v", ev.Mentions)
	}
	if w3 != 2 {
		t.Fatalf("bare @worker-3 should resolve to both hosts, got %+v", ev.Mentions)
	}
	if len(ev.Mentions) != 3 { // 2×worker-3 + all; @nobody-here stays text
		t.Fatalf("unexpected mentions: %+v", ev.Mentions)
	}
}

func TestUnresolvedMentionsReported(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "worker-3"))
	mustJoin(t, h, "r", agentID("anubis", "oracle"))

	// A typo'd name, an unknown host/name pair (trailing period trimmed),
	// a repeat, and an email address — only the first two are reported.
	_, unresolved, err := h.Send("r", a.PID,
		"@orcale and @zeus/ghost. — @orcale again, cc ops@example.com, @oracle", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 || unresolved[0] != "@orcale" || unresolved[1] != "@zeus/ghost" {
		t.Fatalf("unexpected unresolved: %q", unresolved)
	}

	// Code messages never report: @decorators aren't addressing anyone.
	_, unresolved, err = h.Send("r", a.PID, "@property\ndef x(self): ...", proto.KindCode)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("code message reported unresolved %q (err %v)", unresolved, err)
	}
}

func TestMentionTrailingPunctuation(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "can"))
	mustJoin(t, h, "r", agentID("zeus", "claude-observer"))

	// "@zeus/claude-observer." — the regex captures the trailing period
	// into the name; resolution must trim it back. Same for bare names
	// and @room.
	ev, _, err := h.Send("r", a.PID, "@zeus/claude-observer. also @claude-observer- and @room.", "")
	if err != nil {
		t.Fatal(err)
	}
	var host, all int
	for _, m := range ev.Mentions {
		if m.All {
			all++
		}
		if m.Name == "claude-observer" {
			host++
		}
	}
	if host != 1 || all != 1 {
		t.Fatalf("trailing punctuation not trimmed: %+v", ev.Mentions)
	}
	// A name that truly ends in a trimmable char still resolves exactly.
	b := mustJoin(t, h, "r", agentID("zeus", "worker-"))
	ev, _, err = h.Send("r", b.PID, "ping @worker-", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Mentions) != 1 || ev.Mentions[0].Name != "worker-" {
		t.Fatalf("exact-match name ending in '-' should win: %+v", ev.Mentions)
	}
}

func TestIdentityBoundToPID(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "worker-3"))
	if err := h.SetTask("r", a.PID, "new focus"); err != nil {
		t.Fatal(err)
	}
	ev, _, err := h.Send("r", a.PID, "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.From.Task != "new focus" {
		t.Fatalf("task change not reflected in identity snapshot: %+v", ev.From)
	}
}

func TestRateLimitAndBodyCap(t *testing.T) {
	h, clock := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))

	// Burst of 5 goes through; the 6th is refused with a retry hint.
	for i := 0; i < 5; i++ {
		if _, _, err := h.Send("r", a.PID, "m", ""); err != nil {
			t.Fatalf("burst send %d refused: %v", i, err)
		}
	}
	_, _, err := h.Send("r", a.PID, "m", "")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter <= 0 {
		t.Fatalf("expected 429 with retry-after, got %v", err)
	}
	// After 2s at 30/min one token has refilled.
	clock.advance(2 * time.Second)
	if _, _, err := h.Send("r", a.PID, "m", ""); err != nil {
		t.Fatalf("send after refill: %v", err)
	}

	_, _, err = h.Send("r", a.PID, string(make([]byte, 17*1024)), "")
	if !errors.As(err, &ae) || ae.Status != 413 {
		t.Fatalf("expected 413 for oversize body, got %v", err)
	}
}

func TestEvictionAndCursorGone(t *testing.T) {
	h, clock := newTestHub(Config{RoomCap: 10, RatePerMin: 100000, Burst: 100000})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	first, _, err := h.Send("r", a.PID, "first", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		clock.advance(time.Millisecond)
		if _, _, err := h.Send("r", a.PID, "spam", ""); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.EventsAfter("r", a.PID, first.ID)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != proto.ErrCursorGone {
		t.Fatalf("expected cursor_gone after eviction, got %v", err)
	}
}

func TestPIDLifecycle(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	if err := h.Leave("r", a.PID); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.Send("r", a.PID, "hi", "")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != proto.ErrPIDGone {
		t.Fatalf("expected pid_gone after leave, got %v", err)
	}
}

func TestIdleExpiryAndRoomClosed(t *testing.T) {
	h, clock := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))

	clock.advance(9 * time.Hour)
	if expired := h.Sweep(); len(expired) != 1 || expired[0] != "r" {
		t.Fatalf("expected room expiry, got %v", expired)
	}

	// The tombstoned room answers the participant's next poll with
	// room_closed before the following sweep deletes it.
	evs, err := h.EventsAfter("r", a.PID, "")
	if err != nil || len(evs) != 1 || evs[0].Type != proto.TypeRoomClosed {
		t.Fatalf("expected room_closed, got %v err %v", evs, err)
	}

	if h.Sweep(); len(h.Rooms()) != 0 {
		t.Fatal("tombstone not deleted on second sweep")
	}

	// A write to the dead pid is pid_gone; rejoining creates the room fresh.
	_, _, err = h.Send("r", a.PID, "hi", "")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != proto.ErrPIDGone {
		t.Fatalf("expected pid_gone, got %v", err)
	}
	b := mustJoin(t, h, "r", agentID("zeus", "w"))
	if _, _, err := h.Send("r", b.PID, "fresh room", ""); err != nil {
		t.Fatalf("send after recreate: %v", err)
	}
}

func TestActivityKeepsRoomAlive(t *testing.T) {
	h, clock := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	for i := 0; i < 3; i++ {
		clock.advance(5 * time.Hour)
		if _, _, err := h.Send("r", a.PID, "still here", ""); err != nil {
			t.Fatalf("send at hour %d: %v", (i+1)*5, err)
		}
		if expired := h.Sweep(); len(expired) != 0 {
			t.Fatalf("room expired despite activity: %v", expired)
		}
	}
}

func TestParkAndWake(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	b := mustJoin(t, h, "r", agentID("zeus", "x"))

	ch, err := h.Park("r", a.PID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		<-ch
		close(done)
	}()
	if _, _, err := h.Send("r", b.PID, "wake up", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parked poll not woken by send")
	}

	// A newer park supersedes the older one, which wakes immediately.
	ch1, _ := h.Park("r", a.PID)
	ch2, _ := h.Park("r", a.PID)
	select {
	case <-ch1:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded poll not woken")
	}
	select {
	case <-ch2:
		t.Fatal("newer poll should still be parked")
	default:
	}
}

func TestHistory(t *testing.T) {
	h, clock := newTestHub(Config{RatePerMin: 100000, Burst: 100000})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	var ids []string
	for i := 0; i < 10; i++ {
		clock.advance(time.Millisecond)
		ev, _, err := h.Send("r", a.PID, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	evs, err := h.History("r", "", 3)
	if err != nil || len(evs) != 3 || evs[2].ID != ids[9] {
		t.Fatalf("history head: %v err %v", evs, err)
	}
	evs, err = h.History("r", ids[5], 3)
	if err != nil || len(evs) != 3 || evs[2].ID != ids[4] {
		t.Fatalf("history before: got %d events err %v", len(evs), err)
	}
}
