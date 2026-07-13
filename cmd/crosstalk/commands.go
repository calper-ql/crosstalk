package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	token := fs.String("token", "", "the server's global token (required)")
	pos, err := splitArgs("register", args, 3, fs)
	if err != nil {
		return err
	}
	addr, portStr, alias := pos[0], pos[1], pos[2]
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("register: %q is not a valid port", portStr)
	}
	if !client.ValidAlias(alias) {
		return fmt.Errorf("register: alias must be lowercase letters, digits, hyphens (got %q)", alias)
	}
	if *token == "" {
		return errors.New("register: --token is required")
	}

	// Trust-on-first-use: pin whatever certificate the server presents now,
	// then verify the token actually works before writing anything — a bad
	// registration is discovered at register time, never mid-run.
	fp, err := client.FetchFingerprint(addr, port)
	if err != nil {
		return fmt.Errorf("register: cannot reach %s:%d: %w", addr, port, err)
	}
	entry := client.ServerEntry{Addr: addr, Port: port, Token: *token, Fingerprint: fp}
	conn := client.NewConn(entry)
	if _, err := conn.Rooms(); err != nil {
		return fmt.Errorf("register: server reachable but token rejected or API failed: %w", err)
	}
	ver, err := conn.Version()
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	reg.Servers[alias] = entry
	if reg.Default == "" {
		reg.Default = alias
	}
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "registered %q -> %s:%d (server %s, cert pinned)\n", alias, addr, port, ver.Version)
	if reg.Default == alias {
		fmt.Fprintf(os.Stderr, "%q is the default server\n", alias)
	}
	return nil
}

func cmdDefault(args []string) error {
	fs := flag.NewFlagSet("default", flag.ContinueOnError)
	pos, err := splitArgs("default", args, 1, fs)
	if err != nil {
		return err
	}
	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	if _, ok := reg.Servers[pos[0]]; !ok {
		return fmt.Errorf("no server registered under alias %q", pos[0])
	}
	reg.Default = pos[0]
	return reg.Save()
}

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	name := fs.String("name", "", "role designation, e.g. worker-3 (required)")
	task := fs.String("task", "", "one line: what you are doing right now (required)")
	pos, err := splitArgs("join", args, 1, fs)
	if err != nil {
		return err
	}
	if *name == "" || *task == "" {
		return errors.New("join: --name and --task are required in agent mode")
	}

	alias, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	sess, err := doJoin(conn, alias, room, *name, *task)
	if err != nil {
		return err
	}
	ndjson(map[string]string{"pid": sess.PID, "cursor": sess.Cursor, "room": room, "alias": alias})
	return nil
}

// doJoin performs the join request, stamps the hostname, and records the
// session (pid + head cursor) in the sessions file.
func doJoin(conn *client.Conn, alias, room, name, task string) (client.Session, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return client.Session{}, fmt.Errorf("cannot determine hostname: %w", err)
	}
	resp, err := conn.Join(room, proto.JoinRequest{
		Mode: proto.ModeAgent, Hostname: hostname, Name: name, Task: task,
	})
	if err != nil {
		return client.Session{}, err
	}
	sess := client.Session{
		Alias: alias, Room: room, Name: name,
		PID: resp.PID, Cursor: resp.Cursor, JoinedAt: time.Now().UTC(),
	}
	if err := client.UpsertSession(sess); err != nil {
		return client.Session{}, fmt.Errorf("joined (pid %s) but could not record session: %w", resp.PID, err)
	}
	return sess, nil
}

// resolveSession is the shared front half of every writing command.
func resolveSession(ref, asFlag string) (client.Session, *client.Conn, error) {
	alias, room, conn, err := connFor(ref)
	if err != nil {
		return client.Session{}, nil, err
	}
	sess, err := client.ResolvePID(asFlag, alias, room)
	if err != nil {
		return client.Session{}, nil, err
	}
	return sess, conn, nil
}

// pidGone reports whether err is the server telling us our pid is dead,
// and prunes the session entry so the stale pid can't be picked up again.
func pidGone(err error, sess client.Session) bool {
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Code == proto.ErrPIDGone {
		_ = client.PruneSession(sess.PID)
		return true
	}
	return false
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	kind := fs.String("kind", proto.KindText, "text | status | code")
	as := fs.String("as", "", "explicit pid to write as")
	pos, err := splitArgs("send", args, 2, fs)
	if err != nil {
		return err
	}
	body := pos[1]
	if body == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("send: reading stdin: %w", err)
		}
		body = string(data)
	}

	sess, conn, err := resolveSession(pos[0], *as)
	if err != nil {
		return err
	}
	resp, err := conn.Send(sess.Room, sess.PID, body, *kind)
	if err != nil {
		if pidGone(err, sess) {
			return fmt.Errorf("session expired (room gone or server restarted): rejoin with `crosstalk join %s %s--task \"...\"` and resend", pos[0], nameHint(sess))
		}
		return err
	}
	ndjson(map[string]string{"id": resp.ID})
	return nil
}

func cmdTask(args []string) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	set := fs.String("set", "", "new one-line task description (required)")
	as := fs.String("as", "", "explicit pid to write as")
	pos, err := splitArgs("task", args, 1, fs)
	if err != nil {
		return err
	}
	if *set == "" {
		return errors.New("task: --set is required")
	}
	sess, conn, err := resolveSession(pos[0], *as)
	if err != nil {
		return err
	}
	if err := conn.SetTask(sess.Room, sess.PID, *set); err != nil {
		if pidGone(err, sess) {
			return errors.New("session expired: rejoin first")
		}
		return err
	}
	return nil
}

func cmdLeave(args []string) error {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	as := fs.String("as", "", "explicit pid to leave as")
	pos, err := splitArgs("leave", args, 1, fs)
	if err != nil {
		return err
	}
	sess, conn, err := resolveSession(pos[0], *as)
	if err != nil {
		return err
	}
	err = conn.Leave(sess.Room, sess.PID)
	_ = client.PruneSession(sess.PID)
	if err != nil && !isPIDGoneErr(err) { // leaving an already-dead pid is fine
		return err
	}
	return nil
}

func nameHint(sess client.Session) string {
	if sess.Name == "" {
		return "--name <role> "
	}
	return "--name " + sess.Name + " "
}

func isPIDGoneErr(err error) bool {
	var ae *client.APIError
	return errors.As(err, &ae) && ae.Code == proto.ErrPIDGone
}

func cmdRooms(args []string) error {
	fs := flag.NewFlagSet("rooms", flag.ContinueOnError)
	alias := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		alias, args = args[0], args[1:]
	}
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	_, entry, err := reg.Lookup(alias)
	if err != nil {
		return err
	}
	rooms, err := client.NewConn(entry).Rooms()
	if err != nil {
		return err
	}
	for _, r := range rooms {
		ndjson(r)
	}
	return nil
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max events to return")
	before := fs.String("before", "", "return events older than this cursor")
	pos, err := splitArgs("history", args, 1, fs)
	if err != nil {
		return err
	}
	_, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	evs, err := conn.History(room, *before, *limit)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		ndjson(ev)
	}
	return nil
}
