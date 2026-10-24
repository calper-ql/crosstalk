package main

import (
	"bufio"
	"errors"
	"flag"
	"os"
	"slices"
	"time"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

// pollSlice is the per-request hold for the events long-poll; the server
// caps at 30s, we stay under it.
const pollSlice = 25 * time.Second

// cmdWait is the agent's blocking notification primitive. Exit 0: new
// messages were printed. Exit 2: timeout, nothing new, no stdout output.
// Exit 1: something is wrong.
//
// Spec decisions the design doc left open, resolved here:
//   - Default timeout is 0 = no limit: wait blocks until something wakes it.
//     --timeout <s> is a fixed deadline, not an idle timer — an agent sizes
//     it to fit under its tool's own time limit, so activity that doesn't
//     wake it must not extend it.
//   - --mentions-only affects only the wake condition; on wake, all new
//     messages since the cursor are printed, so the mention arrives with
//     its surrounding context.
//   - The persisted cursor advances only on exit 0. A timed-out wait leaves
//     the cursor where it was, so nothing is ever skipped silently.
//   - room_closed always wakes and prints: the room is gone, and an agent
//     must hear that rather than sleep on until its session is rejoined.
func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Int("timeout", 0, "max seconds to block (0 = no limit)")
	mentionsOnly := fs.Bool("mentions-only", false, "wake only when directly addressed (or @room)")
	allEvents := fs.Bool("all-events", false, "wake on and print presence/room_closed too")
	as := fs.String("as", "", asUsage)
	format := formatFlag(fs)
	pos, err := parseArgs("wait", args, 1, 1, fs)
	if err != nil {
		return err
	}
	if *mentionsOnly && *allEvents {
		return errors.New("wait: --mentions-only and --all-events are mutually exclusive")
	}
	if *timeout < 0 {
		return errors.New("wait: --timeout must be 0 (no limit) or a positive number of seconds")
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}

	a, err := resolveAgent(pos[0], *as)
	if err != nil {
		return err
	}
	p.me = a.self()
	if *mentionsOnly && !p.me.known() {
		return errors.New("wait: --mentions-only needs a sessions-file entry to know who you are (join on this machine, or drop --as)")
	}

	cursor := a.session().Cursor
	if cursor == "" {
		if cursor, err = headCursor(a); err != nil {
			return err
		}
	}

	closed := func(ev proto.Event) bool { return ev.Type == proto.TypeRoomClosed }
	// A session's own messages never wake its wait and are not re-printed:
	// send-then-wait is the canonical agent loop, and waking an agent on
	// what it just said would spin the loop. (--all-events includes them,
	// since that mode asks for the raw feed.)
	wakes := func(ev proto.Event) bool {
		switch {
		case *allEvents, closed(ev):
			return true
		case *mentionsOnly:
			return p.me.addressed(ev)
		default:
			return ev.Type == proto.TypeMessage && !p.me.authored(ev)
		}
	}
	shown := func(ev proto.Event) bool {
		return *allEvents || closed(ev) || (ev.Type == proto.TypeMessage && !p.me.authored(ev))
	}

	finite := *timeout > 0
	var deadline time.Time
	if finite {
		deadline = time.Now().Add(time.Duration(*timeout) * time.Second)
	}

	var pending []proto.Event
	for {
		slice := pollSlice
		if finite {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				os.Exit(2)
			}
			// The last poll holds only for what is left, so --timeout 3
			// returns in about 3 seconds, not a full slice.
			if remaining < slice {
				slice = max(remaining.Round(time.Second), time.Second)
			}
		}

		s := a.session()
		evs, err := a.conn.Events(s.Room, s.PID, cursor, slice)
		if err != nil {
			var ae *client.APIError
			switch {
			case errors.As(err, &ae) && ae.Code == proto.ErrCursorGone:
				// The pid is fine but the gap is unverifiable: resume from
				// the present rather than silently backfilling.
				warn("cursor fell off the room buffer; resuming from the present")
				pending = nil
				if cursor, err = headCursor(a); err != nil {
					return err
				}
				continue
			case isPIDGoneErr(err):
				// Server restarted: rejoin and keep waiting. Anything
				// already pending is still context worth printing on wake.
				if err := a.rejoin(s); err != nil {
					return err
				}
				cursor = a.session().Cursor
				continue
			}
			return err
		}
		if len(evs) == 0 {
			continue
		}
		cursor = evs[len(evs)-1].ID
		pending = append(pending, evs...)
		if !slices.ContainsFunc(evs, wakes) {
			continue
		}

		for _, ev := range pending {
			if shown(ev) {
				p.event(ev)
			}
		}
		if slices.ContainsFunc(evs, closed) {
			_ = client.PruneSession(s.PID)
		} else {
			_ = client.AdvanceCursor(s.PID, cursor)
		}
		return nil
	}
}

// headCursor finds the room's current head for a session with no stored
// cursor (pid supplied via --as/env without a matching file entry).
func headCursor(a *agent) (string, error) {
	var head string
	err := a.do(func(s client.Session) error {
		evs, err := a.conn.Events(s.Room, s.PID, "", 0)
		if err != nil {
			return err
		}
		head = ""
		if len(evs) > 0 {
			head = evs[len(evs)-1].ID
		}
		return nil
	})
	return head, err
}

// streamEvents is the shared loop behind listen and session: long-poll,
// print events (all of them when filter is nil), persist the cursor, retry
// transient failures with backoff and a visible error frame, and rejoin
// when the server forgot the session. room_closed and error frames always
// print regardless of filter — a monitor must never be silent about its
// stream dying.
func streamEvents(a *agent, p *printer, cursor string, filter func(proto.Event) bool) error {
	backoff := time.Second
	for {
		s := a.session()
		evs, err := a.conn.Events(s.Room, s.PID, cursor, pollSlice)
		if err != nil {
			var ae *client.APIError
			switch {
			case errors.As(err, &ae) && ae.Code == proto.ErrCursorGone:
				p.errFrame(proto.ErrCursorGone, "cursor fell off the room buffer; resuming from the present")
				if cursor, err = headCursor(a); err != nil {
					return err
				}
				continue
			case isPIDGoneErr(err):
				if err := a.rejoin(s); err != nil {
					p.errFrame(proto.ErrPIDGone, err.Error())
					return err
				}
				p.joined(joinReport(a.conn, a.session(), p.me, "rejoined"))
				cursor = a.session().Cursor
				continue
			default:
				p.errFrame("transient", err.Error())
				time.Sleep(backoff)
				if backoff *= 2; backoff > 15*time.Second {
					backoff = 15 * time.Second
				}
				continue
			}
		}
		backoff = time.Second
		for _, ev := range evs {
			if filter == nil || filter(ev) || ev.Type == proto.TypeRoomClosed {
				p.event(ev)
			}
			if ev.Type == proto.TypeRoomClosed {
				_ = client.PruneSession(s.PID)
				return errors.New("room closed (idle expiry)")
			}
		}
		if len(evs) > 0 {
			cursor = evs[len(evs)-1].ID
			_ = client.AdvanceCursor(s.PID, cursor)
		}
	}
}

// cmdListen streams frames until killed. Three monitor modes so an agent
// can pick its own noise level without a grep pipeline: default = every
// event; --messages-only = message frames only; --mentions-only = only
// messages that address you (or @room), excluding your own. room_closed and
// error frames always come through.
func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	as := fs.String("as", "", asUsage)
	mentionsOnly := fs.Bool("mentions-only", false, "print only messages addressed to you (or @room)")
	messagesOnly := fs.Bool("messages-only", false, "print only message frames (no presence)")
	format := formatFlag(fs)
	pos, err := parseArgs("listen", args, 1, 1, fs)
	if err != nil {
		return err
	}
	if *mentionsOnly && *messagesOnly {
		return errors.New("listen: --mentions-only and --messages-only are mutually exclusive")
	}
	p, err := newPrinter(*format)
	if err != nil {
		return err
	}
	a, err := resolveAgent(pos[0], *as)
	if err != nil {
		return err
	}
	p.me = a.self()

	var filter func(proto.Event) bool
	if *messagesOnly {
		filter = func(ev proto.Event) bool { return ev.Type == proto.TypeMessage }
	}
	if *mentionsOnly {
		if !p.me.known() {
			return errors.New("listen: --mentions-only needs a sessions-file entry to know who you are (join on this machine, or drop --as)")
		}
		filter = p.me.addressed
	}

	cursor := a.session().Cursor
	if cursor == "" {
		if cursor, err = headCursor(a); err != nil {
			return err
		}
	}
	return streamEvents(a, p, cursor, filter)
}

// cmdSession is the bidirectional loop: joins itself, streams events to
// stdout, and posts each stdin line as a message. Stdin EOF leaves the room
// and exits 0.
func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	name := fs.String("name", "", "role designation (required)")
	task := fs.String("task", "", "one line: what you are doing (required)")
	format := formatFlag(fs)
	pos, err := parseArgs("session", args, 1, 1, fs)
	if err != nil {
		return err
	}
	if *name == "" || *task == "" {
		return errors.New("session: --name and --task are required")
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
	a := &agent{conn: conn, ref: pos[0], hostname: hostname, sess: sess}
	p.me = a.self()
	p.joined(joinReport(conn, sess, p.me, "joined"))

	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			var resp proto.SendResponse
			err := a.do(func(s client.Session) (err error) {
				resp, err = sendPaced(a.conn, s, line, proto.KindText)
				return err
			})
			if err != nil {
				p.errFrame("send_failed", err.Error())
				continue
			}
			warnUnresolved(resp.Unresolved, pos[0])
		}
		s := a.session()
		_ = conn.Leave(s.Room, s.PID)
		_ = client.PruneSession(s.PID)
		os.Exit(0)
	}()

	return streamEvents(a, p, sess.Cursor, nil)
}
