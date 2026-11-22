package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

const asUsage = "which of your sessions to act as — role name or pid (when several on this machine share the room)"

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	token := fs.String("token", "", "the server's global token (or set CROSSTALK_TOKEN)")
	fingerprint := fs.String("fingerprint", "", "the server's certificate fingerprint (sha256), to pin it instead of trusting the first one seen")
	pos, err := parseArgs("register", args, 3, 3, fs)
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
	// The environment is the way to pass the token without putting it on a
	// command line, where every user of the machine can read it in `ps`.
	if *token == "" {
		*token = os.Getenv("CROSSTALK_TOKEN")
	}
	if *token == "" {
		return errors.New("register: --token is required (or set CROSSTALK_TOKEN)")
	}
	want := ""
	if *fingerprint != "" {
		if want = client.NormalizeFingerprint(*fingerprint); want == "" {
			return fmt.Errorf("register: --fingerprint must be the sha256 of the server's certificate: 64 hex characters (colons and a sha256: prefix are fine)")
		}
	}

	// Trust-on-first-use, unless a fingerprint was given: pin whatever
	// certificate the server presents now, then verify the token actually
	// works before writing anything — a bad registration is discovered at
	// register time, never mid-run. With --fingerprint there is no first use
	// to trust: the certificate presented must be the one named, and the
	// token is never sent to a server that shows another.
	fp, err := client.FetchFingerprint(addr, port)
	if err != nil {
		return fmt.Errorf("register: cannot reach %s:%d: %w", addr, port, err)
	}
	if want != "" && fp != want {
		return fmt.Errorf("register: %s:%d presents a different certificate than the one given\n  expected %s\n  presented %s\nnothing was registered and the token was not sent", addr, port, want, fp)
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
	how := "cert pinned on first use"
	if want != "" {
		how = "cert matches the given fingerprint"
	}
	fmt.Fprintf(os.Stderr, "registered %q -> %s:%d (server %s, %s)\n", alias, addr, port, ver.Version, how)
	if reg.Default == alias {
		fmt.Fprintf(os.Stderr, "%q is the default server\n", alias)
	}
	return nil
}

func cmdDefault(args []string) error {
	fs := flag.NewFlagSet("default", flag.ContinueOnError)
	pos, err := parseArgs("default", args, 1, 1, fs)
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
	backlog := fs.Int("backlog", 0, "also print the last n messages from before you joined")
	format := formatFlag(fs)
	pos, err := parseArgs("join", args, 1, 1, fs)
	if err != nil {
		return err
	}
	if *name == "" || *task == "" {
		return fmt.Errorf("join: --name and --task are required in agent mode — usage: %s", usageLine("join"))
	}
	if *backlog < 0 {
		return errors.New("join: --backlog must be 0 or more")
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}

	alias, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	sess, err := doJoin(conn, alias, room, *name, *task)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	p.me = self{hostname, sess.Name}
	p.joined(joinReport(conn, sess, p.me, ""))

	// Context from before the join: printed when asked for, otherwise a
	// pointer to it — a late joiner should never miss that there was a
	// conversation already.
	earlier, err := recentMessages(conn, room, sess.Cursor, max(*backlog, 1))
	switch {
	case err != nil:
		warn("could not fetch earlier messages: %v", err)
	case *backlog > 0:
		for _, ev := range earlier {
			p.event(ev)
		}
	case len(earlier) > 0:
		warn("%s already has a conversation — catch up with: crosstalk history %s --messages-only --limit 20",
			client.ShellQuote(pos[0]), client.ShellQuote(pos[0]))
	}

	if others, err := client.SessionsIn(alias, room); err == nil && len(others) > 1 {
		var names []string
		for _, o := range others {
			if o.Name != sess.Name {
				names = append(names, o.Name)
			}
		}
		warn("other sessions on this machine are also in %s (%s): add --as %s to every command for this room",
			client.ShellQuote(pos[0]), strings.Join(names, ", "), sess.Name)
	}
	return nil
}

// joinRequest performs the join and returns the session it opens, with this
// machine's hostname stamped on the identity.
func joinRequest(conn *client.Conn, alias, room, name, task string) (client.Session, error) {
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
	return client.Session{
		Alias: alias, Room: room, Name: name, Task: task,
		PID: resp.PID, Cursor: resp.Cursor, JoinedAt: time.Now().UTC(),
	}, nil
}

// doJoin joins and records the session (pid + head cursor) in the sessions
// file.
func doJoin(conn *client.Conn, alias, room, name, task string) (client.Session, error) {
	sess, err := joinRequest(conn, alias, room, name, task)
	if err != nil {
		return client.Session{}, err
	}
	if err := client.UpsertSession(sess); err != nil {
		return client.Session{}, fmt.Errorf("joined (pid %s) but could not record session: %w", sess.PID, err)
	}
	return sess, nil
}

// joinReport builds join's output line, listing who else is present.
func joinReport(conn *client.Conn, sess client.Session, me self, typ string) joinedInfo {
	info := joinedInfo{Type: typ, Alias: sess.Alias, Room: sess.Room, PID: sess.PID, Cursor: sess.Cursor, You: me.display()}
	parts, err := conn.Participants(sess.Room)
	if err != nil {
		warn("could not list participants: %v", err)
		return info
	}
	info.Present = []string{}
	for _, pi := range parts {
		if d := pi.Identity.Display(); !me.is(&pi.Identity) && !slices.Contains(info.Present, d) {
			info.Present = append(info.Present, d)
		}
	}
	return info
}

// historyPage is the server's cap on one history request.
const historyPage = 500

// recentMessages returns up to n of the room's latest messages strictly
// before the cursor ("" = the present), oldest first, paging back through
// presence events as needed.
func recentMessages(conn *client.Conn, room, before string, n int) ([]proto.Event, error) {
	var out []proto.Event
	for len(out) < n {
		evs, err := conn.History(room, before, historyPage)
		if err != nil {
			return nil, err
		}
		for i := len(evs) - 1; i >= 0 && len(out) < n; i-- {
			if evs[i].Type == proto.TypeMessage {
				out = append(out, evs[i])
			}
		}
		if len(evs) < historyPage {
			break
		}
		before = evs[0].ID
	}
	slices.Reverse(out)
	return out, nil
}

func isPIDGoneErr(err error) bool {
	var ae *client.APIError
	return errors.As(err, &ae) && ae.Code == proto.ErrPIDGone
}

// maxPacingWait is the longest rate-limit refusal send waits out by itself.
const maxPacingWait = 15 * time.Second

// sendPaced posts a message, waiting out short rate-limit refusals instead
// of failing: the limit exists to pace agents to human reading speed, and
// waiting is exactly that.
func sendPaced(conn *client.Conn, s client.Session, body, kind string) (proto.SendResponse, error) {
	for attempt := 0; ; attempt++ {
		resp, err := conn.Send(s.Room, s.PID, body, kind)
		var ae *client.APIError
		if attempt < 3 && errors.As(err, &ae) && ae.Code == proto.ErrRateLimited &&
			ae.RetryAfter > 0 && ae.RetryAfter <= maxPacingWait {
			warn("rate limited — sending in %s", ae.RetryAfter)
			time.Sleep(ae.RetryAfter)
			continue
		}
		return resp, err
	}
}

// warnUnresolved flags mentions that reached no one: a sleeper waiting with
// --mentions-only would otherwise never wake, and both sides would wait.
func warnUnresolved(tokens []string, ref string) {
	for _, t := range tokens {
		warn("warning: %s matched no one in %s, so nobody was notified for it — see who is here with: crosstalk who %s",
			t, ref, client.ShellQuote(ref))
	}
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	kind := fs.String("kind", proto.KindText, "text | status | code")
	as := fs.String("as", "", asUsage)
	format := formatFlag(fs)
	pos, err := parseArgs("send", args, 2, -1, fs)
	if err != nil {
		return err
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	// Unquoted words arrive as separate arguments; they were meant as one
	// body.
	body := strings.Join(pos[1:], " ")
	if len(pos) == 2 && pos[1] == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("send: reading stdin: %w", err)
		}
		body = string(data)
	}

	a, err := resolveAgent(pos[0], *as)
	if err != nil {
		return err
	}
	var resp proto.SendResponse
	err = a.do(func(s client.Session) (err error) {
		resp, err = sendPaced(a.conn, s, body, *kind)
		return err
	})
	if err != nil {
		return err
	}
	text := "sent"
	if len(resp.Mentions) > 0 {
		text += " · notified " + mentionList(resp.Mentions)
	}
	p.emit(resp, text)
	warnUnresolved(resp.Unresolved, pos[0])
	return nil
}

func cmdTask(args []string) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	set := fs.String("set", "", "new one-line task description (required)")
	as := fs.String("as", "", asUsage)
	pos, err := parseArgs("task", args, 1, 1, fs)
	if err != nil {
		return err
	}
	if *set == "" {
		return errors.New("task: --set is required")
	}
	a, err := resolveAgent(pos[0], *as)
	if err != nil {
		return err
	}
	err = a.do(func(s client.Session) error {
		return a.conn.SetTask(s.Room, s.PID, *set)
	})
	if err != nil {
		return err
	}
	// Remembered so an automatic rejoin redeclares the current task.
	return client.SetSessionTask(a.session().PID, *set)
}

func cmdLeave(args []string) error {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	as := fs.String("as", "", asUsage)
	pos, err := parseArgs("leave", args, 1, 1, fs)
	if err != nil {
		return err
	}
	a, err := resolveAgent(pos[0], *as)
	if err != nil {
		return err
	}
	s := a.session()
	err = a.conn.Leave(s.Room, s.PID)
	_ = client.PruneSession(s.PID)
	if err != nil && !isPIDGoneErr(err) { // leaving an already-dead pid is fine
		return err
	}
	return nil
}

// whoEntry is one participant as `who` prints it, led by exactly what to
// type to address them.
type whoEntry struct {
	Mention  string    `json:"mention"`
	Hostname string    `json:"hostname"`
	Name     string    `json:"name"`
	Mode     string    `json:"mode"`
	Task     string    `json:"task,omitempty"`
	Active   bool      `json:"active"`
	LastSeen time.Time `json:"last_seen"`
	You      bool      `json:"you,omitempty"`
}

// activeWindow matches the TUI's occupant list: seen within it = active.
const activeWindow = 5 * time.Minute

func cmdWho(args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	as := fs.String("as", "", asUsage)
	format := formatFlag(fs)
	pos, err := parseArgs("who", args, 1, 1, fs)
	if err != nil {
		return err
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	alias, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	if p.me, err = optionalSelf(*as, alias, room); err != nil {
		return err
	}
	parts, err := conn.Participants(room)
	if err != nil {
		return err
	}
	for _, pi := range parts {
		id := pi.Identity
		e := whoEntry{
			Mention: "@" + id.Display(), Hostname: id.Hostname, Name: id.Name, Mode: id.Mode, Task: id.Task,
			Active: time.Since(pi.LastSeen) < activeWindow, LastSeen: pi.LastSeen, You: p.me.is(&id),
		}
		text := e.Mention
		if e.You {
			text += " (you)"
		}
		if e.Task != "" {
			text += " — " + e.Task
		}
		state := "active"
		if !e.Active {
			state = "idle " + ago(pi.LastSeen)
		}
		p.emit(e, fmt.Sprintf("%s (%s, %s)", text, e.Mode, state))
	}
	return nil
}

// optionalSelf finds the caller's identity in a room for annotating output
// of commands that don't need a session (who, history). Without --as,
// having no session (or several) just means no annotations.
func optionalSelf(as, alias, room string) (self, error) {
	s, err := client.ResolvePID(as, alias, room)
	if err != nil {
		if as != "" {
			return self{}, err
		}
		return self{}, nil
	}
	hostname, _ := os.Hostname()
	return self{hostname, s.Name}, nil
}

func cmdRooms(args []string) error {
	fs := flag.NewFlagSet("rooms", flag.ContinueOnError)
	format := formatFlag(fs)
	pos, err := parseArgs("rooms", args, 0, 1, fs)
	if err != nil {
		return err
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	alias := ""
	if len(pos) == 1 {
		alias = pos[0]
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
		p.emit(r, fmt.Sprintf("%s · %d participant(s) · last activity %s",
			r.Name, r.Participants, r.LastActivity.Local().Format("15:04:05")))
	}
	return nil
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max events (with --messages-only: messages) to return")
	before := fs.String("before", "", "return events older than this cursor")
	messagesOnly := fs.Bool("messages-only", false, "skip presence events")
	as := fs.String("as", "", asUsage)
	format := formatFlag(fs)
	pos, err := parseArgs("history", args, 1, 1, fs)
	if err != nil {
		return err
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	alias, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	if p.me, err = optionalSelf(*as, alias, room); err != nil {
		return err
	}
	var evs []proto.Event
	if *messagesOnly {
		evs, err = recentMessages(conn, room, *before, *limit)
	} else {
		evs, err = conn.History(room, *before, *limit)
	}
	if err != nil {
		return err
	}
	for _, ev := range evs {
		p.event(ev)
	}
	return nil
}

// sessionInfo is one sessions-file entry as `sessions` prints it.
type sessionInfo struct {
	Alias    string    `json:"alias"`
	Room     string    `json:"room"`
	Name     string    `json:"name"`
	PID      string    `json:"pid"`
	Task     string    `json:"task,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
	Status   string    `json:"status,omitempty"` // --prune: alive | gone (removed) | unknown
	Error    string    `json:"error,omitempty"`
}

// cmdSessions lists this machine's sessions; --prune asks each server
// whether the pid is still alive and drops the dead ones, which clears out
// leftovers from crashed agents that would otherwise make room lookups
// ambiguous.
func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	prune := fs.Bool("prune", false, "check each session with its server and remove dead ones")
	format := formatFlag(fs)
	if _, err := parseArgs("sessions", args, 0, 0, fs); err != nil {
		return err
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	var list []client.Session
	if err := client.WithSessions(func(l *[]client.Session) error {
		list = slices.Clone(*l)
		return nil
	}); err != nil {
		return err
	}
	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	for _, s := range list {
		info := sessionInfo{Alias: s.Alias, Room: s.Room, Name: s.Name, PID: s.PID, Task: s.Task, JoinedAt: s.JoinedAt}
		if *prune {
			info.Status, info.Error = probeSession(reg, s)
			if info.Status == "gone" {
				_ = client.PruneSession(s.PID)
			}
		}
		text := fmt.Sprintf("%s/%s · %s (pid %s)", s.Alias, client.ShellQuote(s.Room), s.Name, s.PID)
		if s.Task != "" {
			text += " — " + s.Task
		}
		switch info.Status {
		case "gone":
			text += " [gone, removed]"
		case "unknown":
			text += " [unknown: " + info.Error + "]"
		case "alive":
			text += " [alive]"
		}
		p.emit(info, text)
	}
	return nil
}

// probeSession asks the session's server whether its pid is alive, with a
// non-blocking events read from its own cursor (a cursor that fell off the
// buffer still proves the pid alive).
func probeSession(reg *client.Registry, s client.Session) (status, errMsg string) {
	_, entry, err := reg.Lookup(s.Alias)
	if err != nil {
		return "unknown", err.Error()
	}
	_, err = client.NewConn(entry).Events(s.Room, s.PID, s.Cursor, 0)
	var ae *client.APIError
	switch {
	case err == nil, errors.As(err, &ae) && ae.Code == proto.ErrCursorGone:
		return "alive", ""
	case isPIDGoneErr(err):
		return "gone", ""
	}
	return "unknown", err.Error()
}
