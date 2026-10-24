package proto

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"
)

// ULID implements the canonical 26-character Crockford-base32 ULID:
// 48-bit millisecond timestamp + 80-bit entropy. The generator is monotonic
// within a process — IDs from one server always sort in issue order, which
// is what makes them usable as cursors.

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

type ULIDGen struct {
	mu      sync.Mutex
	lastMS  uint64
	entropy [10]byte
}

func NewULIDGen() *ULIDGen { return &ULIDGen{} }

// New returns the next ULID for the given time. If called twice within the
// same millisecond, the entropy is incremented rather than re-randomized so
// ordering is strictly monotonic.
func (g *ULIDGen) New(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(t.UnixMilli())
	if ms <= g.lastMS {
		// Same (or earlier — clock step) millisecond: keep the stored
		// timestamp and increment entropy to preserve monotonicity.
		ms = g.lastMS
		for i := 9; i >= 0; i-- {
			g.entropy[i]++
			if g.entropy[i] != 0 {
				break
			}
		}
	} else {
		g.lastMS = ms
		if _, err := rand.Read(g.entropy[:]); err != nil {
			panic("proto: crypto/rand failed: " + err.Error())
		}
	}

	var bin [16]byte
	bin[0] = byte(ms >> 40)
	bin[1] = byte(ms >> 32)
	bin[2] = byte(ms >> 24)
	bin[3] = byte(ms >> 16)
	bin[4] = byte(ms >> 8)
	bin[5] = byte(ms)
	copy(bin[6:], g.entropy[:])

	// 128 bits → 26 base32 chars (leading char carries 2 bits).
	var out [26]byte
	// Encode from the least-significant end, 5 bits at a time.
	var acc uint64
	var bits uint
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(bin[i]) << bits
		bits += 8
		for bits >= 5 && pos >= 0 {
			out[pos] = crockford[acc&31]
			acc >>= 5
			bits -= 5
			pos--
		}
	}
	for pos >= 0 {
		out[pos] = crockford[acc&31]
		acc >>= 5
		pos--
	}
	return string(out[:])
}

// NewPID mints an 8-character base32 participant ID.
func NewPID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("proto: crypto/rand failed: " + err.Error())
	}
	out := make([]byte, 8)
	for i, c := range b {
		out[i] = pidAlphabet[int(c)&31]
	}
	return string(out)
}

const pidAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// LooksLikePID reports whether s has the shape NewPID mints, which lets the
// client tell a pid from a role name in --as.
func LooksLikePID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(pidAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
