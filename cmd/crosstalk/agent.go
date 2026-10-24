package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

// self identifies the caller's session within a room: hostname + role name,
// the pair the server stamps on messages and resolves mentions to. A zero
// self (a pid supplied without a sessions-file entry) matches nothing.
type self struct{ hostname, name string }

func (s self) known() bool     { return s.name != "" }
func (s self) display() string { return s.hostname + "/" + s.name }

func (s self) is(id *proto.Identity) bool {
	return s.known() && id != nil && id.Hostname == s.hostname && id.Name == s.name
}

// authored reports whether this session sent the message.
func (s self) authored(ev proto.Event) bool {
	return ev.Type == proto.TypeMessage && s.is(ev.From)
}

// addressed reports whether a message mentions this session or @room. Your
// own messages never count — an @room you sent is not addressed to you.
func (s self) addressed(ev proto.Event) bool {
	if !s.known() || ev.Type != proto.TypeMessage || s.authored(ev) {
		return false
	}
	for _, m := range ev.Mentions {
		if m.All || (m.Hostname == s.hostname && m.Name == s.name) {
			return true
		}
	}
	return false
}

// agent is the resolved session behind every command that acts as you: the
// connection, this machine's hostname, and the session record. It recovers
// from an expired pid by itself — rejoining under the same name with the
// last declared task — so a server restart costs an agent a stderr note
// rather than a failed command. Identity still cannot drift: the rejoin
// reuses the stored name and re-stamps the hostname.
type agent struct {
	conn     *client.Conn
	ref      string // room reference as typed, for hints
	hostname string

	mu   sync.Mutex
	sess client.Session
}

func resolveAgent(ref, as string) (*agent, error) {
	alias, room, conn, err := connFor(ref)
	if err != nil {
		return nil, err
	}
	sess, err := client.ResolvePID(as, alias, room)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("cannot determine hostname: %w", err)
	}
	return &agent{conn: conn, ref: ref, hostname: hostname, sess: sess}, nil
}

func (a *agent) session() client.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

func (a *agent) self() self { return self{a.hostname, a.session().Name} }

// rejoin replaces dead, whose pid the server reported gone. It is a no-op
// if the session was already replaced (by another goroutine here, or — via
// client.RecoverSession — by another process sharing the session).
func (a *agent) rejoin(dead client.Session) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess.PID != dead.PID {
		return nil
	}
	if dead.Name == "" || dead.Task == "" {
		_ = client.PruneSession(dead.PID)
		return fmt.Errorf("session expired (room idle-expired or server restarted): rejoin with `crosstalk join %s %s--task \"...\"`",
			client.ShellQuote(a.ref), nameHint(dead))
	}
	s, err := client.RecoverSession(dead, func() (client.Session, error) {
		return joinRequest(a.conn, dead.Alias, dead.Room, dead.Name, dead.Task)
	})
	if err != nil {
		return fmt.Errorf("session expired and rejoining failed: %w", err)
	}
	a.sess = s
	warn("session expired (server restarted or room idle-expired) — rejoined %s as %s/%s with your last task",
		client.ShellQuote(a.ref), a.hostname, s.Name)
	return nil
}

// do runs op against the current session, rejoining and retrying once if
// the server reports the pid gone.
func (a *agent) do(op func(client.Session) error) error {
	s := a.session()
	err := op(s)
	if !isPIDGoneErr(err) {
		return err
	}
	if err := a.rejoin(s); err != nil {
		return err
	}
	return op(a.session())
}

func nameHint(sess client.Session) string {
	if sess.Name == "" {
		return "--name <role> "
	}
	return "--name " + sess.Name + " "
}
