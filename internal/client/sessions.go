package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// Session is one live join: the pid the server minted and the read cursor,
// keyed by (alias, room, name). Written by `join`, advanced by `wait`,
// pruned when the server reports the pid dead.
type Session struct {
	Alias    string    `toml:"alias"`
	Room     string    `toml:"room"`
	Name     string    `toml:"name"`
	PID      string    `toml:"pid"`
	Cursor   string    `toml:"cursor"`
	JoinedAt time.Time `toml:"joined_at"`
}

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

// ResolvePID implements the fixed resolution order for writing commands:
// an explicit --as flag, then CROSSTALK_PID, then the sessions file — which
// succeeds only if exactly one entry matches the room, and fails loudly
// listing the candidates if several do. The returned Session may be
// synthesized (no cursor) when the pid came from a flag or the environment
// without a matching file entry.
func ResolvePID(asFlag, alias, room string) (Session, error) {
	fromFile := func(pid string) (Session, bool, error) {
		var found Session
		ok := false
		err := WithSessions(func(list *[]Session) error {
			for _, e := range *list {
				if e.PID == pid {
					found, ok = e, true
				}
			}
			return nil
		})
		return found, ok, err
	}

	if pid := asFlag; pid != "" {
		s, ok, err := fromFile(pid)
		if err != nil {
			return Session{}, err
		}
		if ok {
			return s, nil
		}
		return Session{Alias: alias, Room: room, PID: pid}, nil
	}
	if pid := os.Getenv("CROSSTALK_PID"); pid != "" {
		s, ok, err := fromFile(pid)
		if err != nil {
			return Session{}, err
		}
		if ok {
			return s, nil
		}
		return Session{Alias: alias, Room: room, PID: pid}, nil
	}

	var matches []Session
	err := WithSessions(func(list *[]Session) error {
		for _, e := range *list {
			if e.Alias == alias && e.Room == room {
				matches = append(matches, e)
			}
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	switch len(matches) {
	case 0:
		return Session{}, fmt.Errorf("no session for %s/%s: join first (`crosstalk join %s --name <role> --task \"...\"`)", alias, room, room)
	case 1:
		return matches[0], nil
	default:
		var names []string
		for _, m := range matches {
			names = append(names, fmt.Sprintf("%s (pid %s)", m.Name, m.PID))
		}
		return Session{}, fmt.Errorf("multiple sessions for %s/%s: %s — disambiguate with --as <pid> or CROSSTALK_PID",
			alias, room, strings.Join(names, ", "))
	}
}
