package client

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

func testHome(t *testing.T) {
	t.Helper()
	t.Setenv("CROSSTALK_HOME", t.TempDir())
}

func TestConcurrentUpsertsDontLoseEntries(t *testing.T) {
	testHome(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := UpsertSession(Session{
				Alias: "hub", Room: "r", Name: fmt.Sprintf("w%d", i), PID: fmt.Sprintf("pid%d", i),
			})
			if err != nil {
				t.Errorf("upsert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	var n int
	err := WithSessions(func(list *[]Session) error {
		n = len(*list)
		return nil
	})
	if err != nil || n != 50 {
		t.Fatalf("expected 50 sessions, got %d (err %v)", n, err)
	}
}

func TestResolvePIDOrder(t *testing.T) {
	testHome(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(UpsertSession(Session{Alias: "hub", Room: "r", Name: "a", PID: "pid-a", Cursor: "c1"}))

	// Sessions-file path: exactly one match.
	s, err := ResolvePID("", "hub", "r")
	must(err)
	if s.PID != "pid-a" || s.Cursor != "c1" {
		t.Fatalf("unexpected session: %+v", s)
	}

	// Two matches: fail loudly listing candidates.
	must(UpsertSession(Session{Alias: "hub", Room: "r", Name: "b", PID: "pid-b"}))
	if _, err := ResolvePID("", "hub", "r"); err == nil {
		t.Fatal("expected ambiguity error with two sessions")
	}

	// Env pid picks the right entry.
	t.Setenv("CROSSTALK_PID", "pid-b")
	s, err = ResolvePID("", "hub", "r")
	must(err)
	if s.Name != "b" {
		t.Fatalf("env pid resolved wrong session: %+v", s)
	}

	// --as beats the environment; unknown pid synthesizes a bare session.
	s, err = ResolvePID("pid-a", "hub", "r")
	must(err)
	if s.Name != "a" {
		t.Fatalf("--as resolved wrong session: %+v", s)
	}
	s, err = ResolvePID("k7f2m9qx", "hub", "r")
	must(err)
	if s.PID != "k7f2m9qx" || s.Name != "" {
		t.Fatalf("unknown pid-shaped --as should synthesize: %+v", s)
	}

	// --as also takes the role name; an unknown name is an error, not a
	// synthesized pid the server will reject.
	s, err = ResolvePID("b", "hub", "r")
	must(err)
	if s.PID != "pid-b" {
		t.Fatalf("--as by name resolved wrong session: %+v", s)
	}
	if _, err := ResolvePID("nobody", "hub", "r"); err == nil {
		t.Fatal("expected error for an unknown --as name")
	}

	// No match and no hints: join-first error.
	os.Unsetenv("CROSSTALK_PID")
	if _, err := ResolvePID("", "hub", "other"); err == nil {
		t.Fatal("expected join-first error")
	}
}

func TestAdvanceAndPrune(t *testing.T) {
	testHome(t)
	if err := UpsertSession(Session{Alias: "hub", Room: "r", Name: "a", PID: "p1", Cursor: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := AdvanceCursor("p1", "new"); err != nil {
		t.Fatal(err)
	}
	s, err := ResolvePID("", "hub", "r")
	if err != nil || s.Cursor != "new" {
		t.Fatalf("cursor not advanced: %+v err %v", s, err)
	}
	if err := PruneSession("p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePID("", "hub", "r"); err == nil {
		t.Fatal("expected join-first error after prune")
	}
}

func TestSessionNeverUsedForAnotherRoom(t *testing.T) {
	testHome(t)
	if err := UpsertSession(Session{Alias: "hub", Room: "a", Name: "w", PID: "pida0000"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertSession(Session{Alias: "hub", Room: "b", Name: "w", PID: "pidb0000"}); err != nil {
		t.Fatal(err)
	}
	// An explicit pid from another room is an error...
	if _, err := ResolvePID("pida0000", "hub", "b"); err == nil {
		t.Fatal("expected error: --as pid belongs to another room")
	}
	// ...an ambient one is skipped in favour of this room's session.
	t.Setenv("CROSSTALK_PID", "pida0000")
	s, err := ResolvePID("", "hub", "b")
	if err != nil || s.PID != "pidb0000" {
		t.Fatalf("ambient pid from another room should be skipped: %+v err %v", s, err)
	}
}

func TestRecoverSession(t *testing.T) {
	testHome(t)
	dead := Session{Alias: "hub", Room: "r", Name: "w", PID: "old00000", Task: "t"}
	if err := UpsertSession(dead); err != nil {
		t.Fatal(err)
	}
	joins := 0
	join := func() (Session, error) {
		joins++
		return Session{Alias: "hub", Room: "r", Name: "w", PID: fmt.Sprintf("new%05d", joins), Task: "t"}, nil
	}
	s, err := RecoverSession(dead, join)
	if err != nil || s.PID != "new00001" {
		t.Fatalf("recover: %+v err %v", s, err)
	}
	// A second process recovering the same dead pid adopts the replacement.
	s, err = RecoverSession(dead, join)
	if err != nil || s.PID != "new00001" || joins != 1 {
		t.Fatalf("second recovery should adopt, got %+v (joins %d, err %v)", s, joins, err)
	}
	// The old pid still resolves, to the new session.
	s, err = ResolvePID("old00000", "hub", "r")
	if err != nil || s.PID != "new00001" {
		t.Fatalf("superseded pid should resolve to its replacement: %+v err %v", s, err)
	}
}
