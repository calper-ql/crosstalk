package client

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Session is one live join: the pid the server minted and the read cursor,
// keyed by (alias, room, name). Written by `join`, advanced by `wait`,
// pruned when the server reports the pid dead. Task is the last declared
// task, kept so an expired pid can be replaced by rejoining under the same
// identity; Superseded lists the pids such rejoins replaced, so a pid an
// orchestrator handed out (--as, CROSSTALK_PID) keeps resolving.
type Session struct {
	Alias      string    `toml:"alias"`
	Room       string    `toml:"room"`
	Name       string    `toml:"name"`
	PID        string    `toml:"pid"`
	Cursor     string    `toml:"cursor"`
	Task       string    `toml:"task,omitempty"`
	Superseded []string  `toml:"superseded,omitempty"`
	JoinedAt   time.Time `toml:"joined_at"`
}

// answersTo reports whether pid is this session's, now or before a rejoin.
func (s Session) answersTo(pid string) bool {
	return s.PID == pid || slices.Contains(s.Superseded, pid)
}

// maxSuperseded bounds the rejoin trail kept per session.
const maxSuperseded = 8

type sessionsFile struct {
	Session []Session `toml:"session"`
}

// WithSessions runs fn over the decoded sessions file under an exclusive
// flock, then persists whatever fn leaves in the slice. The lock is what
// lets concurrent one-shot invocations share a session without racing the
// cursor.
//
// The lock lives on a separate never-replaced lock file, and the rewrite is
// tmp+rename: a process killed mid-write leaves either the old or the new
// complete file, never a torn one.
func WithSessions(fn func(*[]Session) error) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "sessions.toml")

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	var sf sessionsFile
	if _, err := toml.DecodeFile(path, &sf); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if err := fn(&sf.Session); err != nil {
		return err
	}

	tmp, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(tmp).Encode(sessionsFile{Session: sf.Session}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// UpsertSession replaces any session with the same (alias, room, name) key.
func UpsertSession(s Session) error {
	return WithSessions(func(list *[]Session) error {
		out := (*list)[:0]
		for _, e := range *list {
			if !(e.Alias == s.Alias && e.Room == s.Room && e.Name == s.Name) {
				out = append(out, e)
			}
		}
		*list = append(out, s)
		return nil
	})
}

// AdvanceCursor persists a new read cursor for the session holding pid.
func AdvanceCursor(pid, cursor string) error {
	return WithSessions(func(list *[]Session) error {
		for i := range *list {
			if (*list)[i].PID == pid {
				(*list)[i].Cursor = cursor
			}
		}
		return nil
	})
}

// SetSessionTask records a newly declared task for the session holding pid.
func SetSessionTask(pid, task string) error {
	return WithSessions(func(list *[]Session) error {
		for i := range *list {
			if (*list)[i].PID == pid {
				(*list)[i].Task = task
			}
		}
		return nil
	})
}

// RecoverSession replaces a session whose pid the server reported gone,
// holding the sessions lock throughout so that concurrent processes on one
// session converge: if another process already rejoined, its replacement is
// adopted rather than joining a second time. join performs the rejoin; when
// it fails the dead entry is pruned and the error returned.
func RecoverSession(dead Session, join func() (Session, error)) (Session, error) {
	var out Session
	var joinErr error
	err := WithSessions(func(list *[]Session) error {
		for _, e := range *list {
			if e.Alias == dead.Alias && e.Room == dead.Room && e.Name == dead.Name &&
				e.PID != dead.PID && slices.Contains(e.Superseded, dead.PID) {
				out = e
				return nil
			}
		}
		kept := (*list)[:0]
		for _, e := range *list {
			if e.PID != dead.PID && !(e.Alias == dead.Alias && e.Room == dead.Room && e.Name == dead.Name) {
				kept = append(kept, e)
			}
		}
		*list = kept
		s, err := join()
		if err != nil {
			joinErr = err
			return nil // still persist the prune
		}
		s.Superseded = append(slices.Clone(dead.Superseded), dead.PID)
		if n := len(s.Superseded); n > maxSuperseded {
			s.Superseded = s.Superseded[n-maxSuperseded:]
		}
		*list = append(*list, s)
		out = s
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return out, joinErr
}

// PruneSession drops a session whose pid the server reported dead.
func PruneSession(pid string) error {
	return WithSessions(func(list *[]Session) error {
		out := (*list)[:0]
		for _, e := range *list {
			if e.PID != pid {
				out = append(out, e)
			}
		}
		*list = out
		return nil
	})
}

// ResolvePID implements the fixed resolution order for session-bound
// commands: an explicit --as, then CROSSTALK_PID, then the sessions file —
// which succeeds only if exactly one entry matches the room, and fails
// loudly listing the candidates if several do.
//
// --as takes a pid or, easier for an agent to carry between calls, the role
// name it joined with. A session from the file is only ever used for its
// own room — never silently to write somewhere else. A pid the file doesn't
// know (handed over by an orchestrator) yields a synthesized Session with no
// name or cursor. CROSSTALK_PID is ambient, so one belonging to another
// room is skipped rather than an error.
func ResolvePID(asFlag, alias, room string) (Session, error) {
	var all []Session
	if err := WithSessions(func(list *[]Session) error {
		all = slices.Clone(*list)
		return nil
	}); err != nil {
		return Session{}, err
	}
	inRoom := func(s Session) bool { return s.Alias == alias && s.Room == room }

	// explicit resolves a pid or role name; ok=false means "skip" (an
	// ambient pid that belongs to another room).
	explicit := func(who string, ambient bool) (s Session, ok bool, err error) {
		for _, e := range all {
			if inRoom(e) && e.answersTo(who) {
				return e, true, nil
			}
		}
		for _, e := range all {
			if inRoom(e) && e.Name == who {
				return e, true, nil
			}
		}
		for _, e := range all {
			if e.answersTo(who) {
				if ambient {
					return Session{}, false, nil
				}
				return Session{}, false, fmt.Errorf("pid %s is your session in %s/%s, not %s/%s", who, e.Alias, e.Room, alias, room)
			}
		}
		if !proto.LooksLikePID(who) {
			return Session{}, false, fmt.Errorf("no session named %q in %s/%s on this machine: join first (`crosstalk join %s --name %s --task \"...\"`)",
				who, alias, room, ShellQuote(room), who)
		}
		return Session{Alias: alias, Room: room, PID: who}, true, nil
	}

	if asFlag != "" {
		s, _, err := explicit(asFlag, false)
		return s, err
	}
	if pid := os.Getenv("CROSSTALK_PID"); pid != "" {
		if s, ok, err := explicit(pid, true); ok || err != nil {
			return s, err
		}
	}

	var matches []Session
	for _, e := range all {
		if inRoom(e) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return Session{}, fmt.Errorf("no session for %s/%s: join first (`crosstalk join %s --name <role> --task \"...\"`)", alias, room, ShellQuote(room))
	case 1:
		return matches[0], nil
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return Session{}, fmt.Errorf("several sessions on this machine share %s/%s (%s): say which one you are with --as <name>, e.g. --as %s",
			alias, room, strings.Join(names, ", "), matches[0].Name)
	}
}

// SessionsIn returns the sessions file entries for one room.
func SessionsIn(alias, room string) ([]Session, error) {
	var out []Session
	err := WithSessions(func(list *[]Session) error {
		for _, e := range *list {
			if e.Alias == alias && e.Room == room {
				out = append(out, e)
			}
		}
		return nil
	})
	return out, err
}

// ShellQuote renders s for a copy-pasteable command hint, single-quoting it
// when it holds anything a shell might act on — room names like ai#42 break
// under zsh's extendedglob when bare.
func ShellQuote(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-:=@%+,", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
