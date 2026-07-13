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
	s, err = ResolvePID("pid-x", "hub", "r")
	must(err)
	if s.PID != "pid-x" || s.Name != "" {
		t.Fatalf("unknown --as should synthesize: %+v", s)
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
