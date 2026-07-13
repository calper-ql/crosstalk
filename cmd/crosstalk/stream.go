package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
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
//   - --mentions-only affects only the wake condition; on wake, all new
//     messages since the cursor are printed, so the mention arrives with
//     its surrounding context.
//   - The persisted cursor advances only on exit 0. A timed-out wait leaves
//     the cursor where it was, so nothing is ever skipped silently.
func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Int("timeout", 300, "max seconds to block")
	mentionsOnly := fs.Bool("mentions-only", false, "wake only when directly addressed (or @room)")
	allEvents := fs.Bool("all-events", false, "wake on and print presence/room_closed too")
	as := fs.String("as", "", "explicit pid to wait as")
	pos, err := splitArgs("wait", args, 1, fs)
	if err != nil {
		return err
	}
	if *mentionsOnly && *allEvents {
		return errors.New("wait: --mentions-only and --all-events are mutually exclusive")
	}

	sess, conn, err := resolveSession(pos[0], *as)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if *mentionsOnly && sess.Name == "" {
		return errors.New("wait: --mentions-only needs a sessions-file entry to know who you are (join on this machine, or drop --as)")
	}

	cursor := sess.Cursor
	if cursor == "" {
		if cursor, err = headCursor(conn, sess); err != nil {
			return err
		}
	}

	addressedToMe := func(ev proto.Event) bool {
		for _, m := range ev.Mentions {
			if m.All || (m.Hostname == hostname && m.Name == sess.Name) {
				return true
			}
		}
		return false
	}
	// A session's own messages never wake its wait and are not re-printed:
	// send-then-wait is the canonical agent loop, and waking an agent on
	// what it just said would spin the loop. (--all-events includes them,
	// since that mode asks for the raw feed.)
	mine := func(ev proto.Event) bool {
		return ev.Type == proto.TypeMessage && ev.From != nil && sess.Name != "" &&
			ev.From.Hostname == hostname && ev.From.Name == sess.Name
	}
	wakes := func(ev proto.Event) bool {
		switch {
		case *allEvents:
			return true
		case *mentionsOnly:
			return ev.Type == proto.TypeMessage && addressedToMe(ev)
		default:
			return ev.Type == proto.TypeMessage && !mine(ev)
		}
	}

	var pending []proto.Event
	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			os.Exit(2)
		}
		slice := pollSlice
		if remaining < slice {
			slice = remaining.Round(time.Second)
			if slice < time.Second {
				slice = time.Second
			}
		}

		evs, err := conn.Events(sess.Room, sess.PID, cursor, slice)
		if err != nil {
			var ae *client.APIError
			if errors.As(err, &ae) && ae.Code == proto.ErrCursorGone {
				// The pid is fine but the gap is unverifiable: resume from
				// the present rather than silently backfilling.
				fmt.Fprintln(os.Stderr, "crosstalk: cursor fell off the room buffer; resuming from the present")
				pending = nil
				if cursor, err = headCursor(conn, sess); err != nil {
					return err
				}
				continue
			}
			if pidGone(err, sess) {
				return fmt.Errorf("session expired: rejoin with `crosstalk join %s %s--task \"...\"`", pos[0], nameHint(sess))
			}
			return err
		}
		if len(evs) > 0 {
			cursor = evs[len(evs)-1].ID
			pending = append(pending, evs...)
		}

		woken := false
		for _, ev := range evs {
			if wakes(ev) {
				woken = true
				break
			}
		}
		if woken {
			for _, ev := range pending {
				if *allEvents || (ev.Type == proto.TypeMessage && !mine(ev)) {
					ndjson(ev)
				}
			}
			_ = client.AdvanceCursor(sess.PID, cursor)
			return nil
		}
	}
}

// headCursor finds the room's current head for a session with no stored
// cursor (pid supplied via --as/env without a matching file entry).
func headCursor(conn *client.Conn, sess client.Session) (string, error) {
	evs, err := conn.Events(sess.Room, sess.PID, "", 0)
	if err != nil {
		if pidGone(err, sess) {
			return "", errors.New("session expired: rejoin first")
		}
		return "", err
	}
	if len(evs) == 0 {
		return "", nil
	}
	return evs[len(evs)-1].ID, nil
}

// errLine emits a machine-parseable error frame on stdout for the streaming
// loops, per the fail-loudly rule: nothing is ever silently buffered.
func errLine(code, msg string) {
	ndjson(map[string]string{"type": "error", "error": code, "message": msg})
}

// streamEvents is the shared loop behind listen and session: long-poll,
// print events as NDJSON (all of them when filter is nil), persist the
// cursor, retry transient failures with backoff and a visible error frame.
// room_closed and error frames always print regardless of filter — a
// monitor must never be silent about its stream dying.
func streamEvents(conn *client.Conn, sess client.Session, cursor string, filter func(proto.Event) bool) error {
	backoff := time.Second
	for {
		evs, err := conn.Events(sess.Room, sess.PID, cursor, pollSlice)
		if err != nil {
			var ae *client.APIError
			switch {
			case errors.As(err, &ae) && ae.Code == proto.ErrCursorGone:
				errLine(proto.ErrCursorGone, "cursor fell off the room buffer; resuming from the present")
				if cursor, err = headCursor(conn, sess); err != nil {
					return err
				}
				continue
			case errors.As(err, &ae) && ae.Code == proto.ErrPIDGone:
				_ = client.PruneSession(sess.PID)
				errLine(proto.ErrPIDGone, "session expired: rejoin and redeclare your task")
				return errors.New("session expired")
			default:
				errLine("transient", err.Error())
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
				ndjson(ev)
			}
			if ev.Type == proto.TypeRoomClosed {
				_ = client.PruneSession(sess.PID)
				return errors.New("room closed (idle expiry)")
			}
		}
		if len(evs) > 0 {
			cursor = evs[len(evs)-1].ID
			_ = client.AdvanceCursor(sess.PID, cursor)
		}
	}
}

// cmdListen streams NDJSON frames until killed. Three monitor modes so an
// agent can pick its own noise level without a grep pipeline:
// default = every event; --messages-only = message frames only;
// --mentions-only = only messages that address you (or @room), excluding
// your own. room_closed and error frames always come through.
func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	as := fs.String("as", "", "explicit pid to listen as")
	mentionsOnly := fs.Bool("mentions-only", false, "print only messages addressed to you (or @room)")
	messagesOnly := fs.Bool("messages-only", false, "print only message frames (no presence)")
	pos, err := splitArgs("listen", args, 1, fs)
	if err != nil {
		return err
	}
	if *mentionsOnly && *messagesOnly {
		return errors.New("listen: --mentions-only and --messages-only are mutually exclusive")
	}
	sess, conn, err := resolveSession(pos[0], *as)
	if err != nil {
		return err
	}

	var filter func(proto.Event) bool
	if *messagesOnly {
		filter = func(ev proto.Event) bool { return ev.Type == proto.TypeMessage }
	}
	if *mentionsOnly {
		if sess.Name == "" {
			return errors.New("listen: --mentions-only needs a sessions-file entry to know who you are (join on this machine, or drop --as)")
		}
		hostname, _ := os.Hostname()
		filter = func(ev proto.Event) bool {
			if ev.Type != proto.TypeMessage {
				return false
			}
			if ev.From != nil && ev.From.Hostname == hostname && ev.From.Name == sess.Name {
				return false // own messages never count as being addressed
			}
			for _, mn := range ev.Mentions {
				if mn.All || (mn.Hostname == hostname && mn.Name == sess.Name) {
					return true
				}
			}
			return false
		}
	}

	cursor := sess.Cursor
	if cursor == "" {
		if cursor, err = headCursor(conn, sess); err != nil {
			return err
		}
	}
	return streamEvents(conn, sess, cursor, filter)
}

// cmdSession is the bidirectional loop: joins itself, streams events to
// stdout as NDJSON, and posts each stdin line as a message. Stdin EOF
// leaves the room and exits 0.
func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	name := fs.String("name", "", "role designation (required)")
	task := fs.String("task", "", "one line: what you are doing (required)")
	pos, err := splitArgs("session", args, 1, fs)
	if err != nil {
		return err
	}
	if *name == "" || *task == "" {
		return errors.New("session: --name and --task are required")
	}

	alias, room, conn, err := connFor(pos[0])
	if err != nil {
		return err
	}
	sess, err := doJoin(conn, alias, room, *name, *task)
	if err != nil {
		return err
	}
	ndjson(map[string]string{"type": "joined", "pid": sess.PID, "room": room, "alias": alias})

	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			if _, err := conn.Send(sess.Room, sess.PID, line, proto.KindText); err != nil {
				errLine("send_failed", err.Error())
			}
		}
		_ = conn.Leave(sess.Room, sess.PID)
		_ = client.PruneSession(sess.PID)
		os.Exit(0)
	}()

	return streamEvents(conn, sess, sess.Cursor, nil)
}
