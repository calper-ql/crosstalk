package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Output formats. NDJSON is the contract for programs; text renders the
// same information as compact chat lines for a model reading its own tool
// output, at a fraction of the tokens.
const (
	formatJSON = "json"
	formatText = "text"
)

func formatFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("CROSSTALK_FORMAT")
	if def == "" {
		def = formatJSON
	}
	return fs.String("format", def, "output: json (NDJSON) or text (compact lines); $CROSSTALK_FORMAT sets the default")
}

// printer writes a command's stdout in the chosen format. me, when known,
// lets it mark messages to and from the caller.
type printer struct {
	text bool
	me   self
}

func newPrinter(format string) (*printer, error) {
	switch format {
	case formatJSON:
		return &printer{}, nil
	case formatText:
		return &printer{text: true}, nil
	}
	return nil, fmt.Errorf("--format must be json or text (got %q)", format)
}

// emit prints v as NDJSON, or the given line in text mode.
func (p *printer) emit(v any, text string) {
	if !p.text {
		ndjson(v)
		return
	}
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Println(text)
}

// shownEvent is an event as the CLI prints it: the wire event plus
// annotations relative to the caller's own session.
type shownEvent struct {
	proto.Event
	ToYou   bool `json:"to_you,omitempty"`
	FromYou bool `json:"from_you,omitempty"`
}

func (p *printer) event(ev proto.Event) {
	p.emit(shownEvent{ev, p.me.addressed(ev), p.me.authored(ev)}, renderEvent(ev, p.me))
}

// errFrame reports a problem inside a stream (listen, session) on stdout,
// per the fail-loudly rule: nothing is ever silently buffered.
func (p *printer) errFrame(code, msg string) {
	p.emit(map[string]string{"type": "error", "error": code, "message": msg}, "! "+code+": "+msg)
}

// joinedInfo is join's report: the session, who you are as others see and
// address you, and who else is in the room.
type joinedInfo struct {
	Type    string   `json:"type,omitempty"` // "joined"/"rejoined" inside streams
	Alias   string   `json:"alias"`
	Room    string   `json:"room"`
	PID     string   `json:"pid"`
	Cursor  string   `json:"cursor,omitempty"`
	You     string   `json:"you"`
	Present []string `json:"present"` // null when the listing failed
}

func (p *printer) joined(info joinedInfo) {
	verb := "joined"
	if info.Type == "rejoined" {
		verb = "session expired — rejoined"
	}
	text := fmt.Sprintf("%s %s as %s", verb, info.Room, info.You)
	switch {
	case info.Present == nil:
	case len(info.Present) == 0:
		text += " — nobody else here yet"
	default:
		text += " — also here: " + strings.Join(info.Present, ", ")
	}
	p.emit(info, text)
}

// renderEvent is the text form of one event. Continuation lines of a body
// are indented, so a message can never forge what looks like a line from
// another sender.
func renderEvent(ev proto.Event, me self) string {
	ts := ev.TS.Local().Format("15:04:05")
	switch ev.Type {
	case proto.TypeMessage:
		who := "?"
		if ev.From != nil {
			who = ev.From.Display()
		}
		if me.authored(ev) {
			who += " (you)"
		}
		if ev.Kind != "" && ev.Kind != proto.KindText {
			who += " [" + ev.Kind + "]"
		}
		if me.addressed(ev) {
			who += " → you"
		}
		body := strings.TrimRight(ev.Body, "\n")
		if !strings.Contains(body, "\n") {
			return fmt.Sprintf("[%s] %s: %s", ts, who, body)
		}
		return fmt.Sprintf("[%s] %s:\n    %s", ts, who, strings.ReplaceAll(body, "\n", "\n    "))
	case proto.TypePresence:
		who, task := "?", ""
		if ev.Who != nil {
			who, task = ev.Who.Display(), ev.Who.Task
		}
		if me.is(ev.Who) {
			who += " (you)"
		}
		line := who + " " + ev.Event
		switch ev.Event {
		case proto.PresenceJoin:
			line = who + " joined"
			if task != "" {
				line += " — " + task
			}
		case proto.PresenceLeave:
			line = who + " left"
		case proto.PresenceTask:
			line = who + " is now: " + task
		}
		return fmt.Sprintf("[%s] · %s", ts, line)
	case proto.TypeRoomClosed:
		return fmt.Sprintf("[%s] · room closed (%s) — rejoin only if you still need it", ts, ev.Reason)
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func mentionList(ms []proto.Mention) string {
	var out []string
	for _, m := range ms {
		if m.All {
			out = append(out, "@room (everyone)")
		} else {
			out = append(out, m.Hostname+"/"+m.Name)
		}
	}
	return strings.Join(out, ", ")
}

// ago renders a short "how long since" for presence.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
