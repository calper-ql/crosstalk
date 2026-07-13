package proto

import (
	"sort"
	"testing"
	"time"
)

func TestULIDMonotonicAndSortable(t *testing.T) {
	g := NewULIDGen()
	now := time.Now()
	var ids []string
	for i := 0; i < 5000; i++ {
		// Hammer the same millisecond plus some forward steps.
		ids = append(ids, g.New(now.Add(time.Duration(i/100)*time.Millisecond)))
	}
	for i, id := range ids {
		if len(id) != 26 {
			t.Fatalf("id %d has length %d: %q", i, len(id), id)
		}
		if i > 0 && !(ids[i-1] < id) {
			t.Fatalf("not strictly increasing at %d: %q then %q", i, ids[i-1], id)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids not lexicographically sorted")
	}
}

func TestULIDClockStepBackwards(t *testing.T) {
	g := NewULIDGen()
	now := time.Now()
	a := g.New(now)
	b := g.New(now.Add(-time.Hour)) // clock stepped back
	if !(a < b) {
		t.Fatalf("monotonicity violated on clock step: %q then %q", a, b)
	}
}

func TestNewPID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		p := NewPID()
		if len(p) != 8 {
			t.Fatalf("pid length %d: %q", len(p), p)
		}
		if seen[p] {
			t.Fatalf("duplicate pid %q", p)
		}
		seen[p] = true
	}
}
