package tui

import (
	"testing"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// Only "/file" and "/save" are commands. Everything else a person types —
// a path, a regex, "/files are nice" — is a message and must be sent as one.
func TestSlashCommand(t *testing.T) {
	for _, c := range []struct {
		line           string
		cmd, arg, rest string
		ok             bool
	}{
		{"/file ./shot.png", "/file", "./shot.png", "", true},
		{"/file ./shot.png the screen after the fix", "/file", "./shot.png", "the screen after the fix", true},
		{`/file "my shots/run 7.png" look at this`, "/file", "my shots/run 7.png", "look at this", true},
		{`/file "unclosed quote.png`, "/file", `"unclosed`, "quote.png", true},
		{"  /file   a.txt  ", "/file", "a.txt", "", true},
		{"/file", "/file", "", "", true},
		{"/save", "/save", "", "", true},
		{"/save 01JXYZ", "/save", "01JXYZ", "", true},
		{"/files are nice", "", "", "", false},
		{"/etc/hosts is wrong on zeus", "", "", "", false},
		{"look at /file here", "", "", "", false},
		{"/saved it", "", "", "", false},
		{"", "", "", "", false},
	} {
		cmd, arg, rest, ok := slashCommand(c.line)
		if cmd != c.cmd || arg != c.arg || rest != c.rest || ok != c.ok {
			t.Errorf("slashCommand(%q) = %q %q %q %v, want %q %q %q %v", c.line, cmd, arg, rest, ok, c.cmd, c.arg, c.rest, c.ok)
		}
	}
}

func TestFileLineAndLastFile(t *testing.T) {
	// A name is cleaned before it is drawn, whatever the server sent.
	got := fileLine(&proto.File{ID: "01A", Name: "a\x1b[31m\nb.png", Size: 2048, Type: "image/png"})
	if want := "  ⎘ a[31mb.png · 2.0 KiB · image/png · /save 01A"; got != want {
		t.Errorf("fileLine = %q, want %q", got, want)
	}
	evs := []proto.Event{
		{Type: proto.TypeMessage, File: &proto.File{ID: "01A"}},
		{Type: proto.TypeMessage, File: &proto.File{ID: "01B"}},
		{Type: proto.TypeMessage, Body: "no file"},
	}
	if id := lastFileID(evs); id != "01B" {
		t.Errorf("lastFileID = %q, want the newest file's", id)
	}
	if id := lastFileID(evs[2:]); id != "" {
		t.Errorf("lastFileID with no file = %q", id)
	}
}
