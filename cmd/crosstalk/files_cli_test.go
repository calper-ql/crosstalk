package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/server"
)

// One agent sends a file, another sees it in its wait and fetches the same
// bytes: the whole of what file sharing is for an agent.
func TestSendingAndFetchingAFile(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	host := hostname(t)
	a, b := newHome(t, ts), newHome(t, ts)
	a.ok(t, "join", room, "--name", "worker", "--task", "t")
	b.ok(t, "join", room, "--name", "oracle", "--task", "t")

	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0xAB, 0x00, 0x7F}, 100_000)...)
	src := filepath.Join(t.TempDir(), "run 7 — écran.png")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// send: the answer describes the file as the server holds it.
	s := frames(t, a.ok(t, "send", room, "--file", src, "@oracle the screen").stdout)[0]
	f, _ := s["file"].(map[string]any)
	if f == nil || f["name"] != "run 7 — écran.png" || f["type"] != "image/png" || int(f["size"].(float64)) != len(data) {
		t.Fatalf("send --file answer: %v", s)
	}
	id := f["id"].(string)

	// The other side's wait: one message, addressed to it, carrying the file.
	ev := frames(t, b.ok(t, "wait", room, "--timeout", "10").stdout)[0]
	ef, _ := ev["file"].(map[string]any)
	if ev["body"] != "@oracle the screen" || ev["to_you"] != true || ef == nil || ef["id"] != id {
		t.Fatalf("wait should print the message with its file: %v", ev)
	}

	// fetch: saved under the client's own folder, under a name that begins
	// with the id and has nothing in it a shell would split or act on, with
	// the bytes that were sent. What the file was called is reported beside it.
	got := frames(t, b.ok(t, "fetch", room, id).stdout)[0]
	path := got["path"].(string)
	if want := filepath.Join(b.dir, "files", id+"-run_7_écran.png"); path != want {
		t.Fatalf("fetch saved to %q, want %q", path, want)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("the fetched file differs from the one sent (%d bytes, err %v)", len(saved), err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("the fetched file is %v, want 0600", st.Mode().Perm())
	}
	if got["name"] != "run 7 — écran.png" || got["type"] != "image/png" || got["id"] != id {
		t.Fatalf("fetch answer: %v", got)
	}

	// --out: a new file where the caller says; never over one that exists,
	// never into a directory under the sender's name.
	dir := t.TempDir()
	out := filepath.Join(dir, "screen.png")
	b.ok(t, "fetch", room, id, "--out", out)
	if saved, _ := os.ReadFile(out); !bytes.Equal(saved, data) {
		t.Fatal("--out wrote something else")
	}
	if r := b.run(t, "fetch", room, id, "--out", out); r.code != 1 || !strings.Contains(r.stderr, "already there") {
		t.Fatalf("fetch over an existing file: exit %d, %s", r.code, r.stderr)
	}
	if r := b.run(t, "fetch", room, id, "--out", dir); r.code != 1 || !strings.Contains(r.stderr, "is a directory") {
		t.Fatalf("fetch into a directory: exit %d, %s", r.code, r.stderr)
	}

	// Text format: the file is named in the head of the line, with its id.
	r := b.ok(t, "history", room, "--messages-only", "--format", "text")
	want := host + `/worker [file ` + id + ` "run 7 — écran.png" 293.0 KiB image/png] → you: @oracle the screen`
	if !strings.Contains(r.stdout, want) {
		t.Fatalf("text history:\n%s\nwant a line with:\n%s", r.stdout, want)
	}

	// A file needs no caption, and a caption alone is still just a message.
	a.ok(t, "send", room, "--file", src)
	if r := a.run(t, "send", room); r.code != 1 || !strings.Contains(r.stderr, "nothing to send") {
		t.Fatalf("send with neither body nor file: exit %d, %s", r.code, r.stderr)
	}
	if r := a.run(t, "send", room, "--file", src, "--kind", "code", "x"); r.code != 1 || !strings.Contains(r.stderr, "--kind does not go with --file") {
		t.Fatalf("send --file --kind code: exit %d, %s", r.code, r.stderr)
	}

	// An id nobody sent.
	if r := b.run(t, "fetch", room, "01NOSUCHFILE"); r.code != 1 || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("fetch of an unknown id: exit %d, %s", r.code, r.stderr)
	}
}

// A file's name is the sender's to choose, and the path fetch prints is read
// by an agent whose next step is a command with that path in it. So the path
// never carries anything a shell would act on.
func TestAFetchedFilesPathIsSafeToType(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a, b := newHome(t, ts), newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	dir := t.TempDir()
	for _, name := range []string{
		"a$(touch pwned).log", "b`id`.txt", "c; rm -rf ~ #.txt", "d | sh", "e && f", `g"quote'.txt`, "h>out.txt", "-rf", "--help", "{i,j}*?.txt", "~root",
	} {
		src := filepath.Join(dir, name)
		if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		sent := frames(t, a.ok(t, "send", room, "--file", src).stdout)[0]["file"].(map[string]any)
		got := frames(t, b.ok(t, "fetch", room, sent["id"].(string)).stdout)[0]
		path := got["path"].(string)
		if strings.ContainsAny(path, " \t\n$`;|&<>(){}[]*?!~'\"\\#") {
			t.Errorf("the file %q was saved at %q", name, path)
		}
		if base := filepath.Base(path); !strings.HasPrefix(base, sent["id"].(string)+"-") || filepath.Dir(path) != filepath.Join(b.dir, "files") {
			t.Errorf("the file %q was saved at %q, not as <id>-<name> in the client's own folder", name, path)
		}
		if got["name"] != name {
			t.Errorf("the name reported for %q is %q", name, got["name"])
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "x" {
			t.Errorf("the file %q at %q: %q %v", name, path, data, err)
		}
		time.Sleep(2100 * time.Millisecond / 5) // within the room's pace
	}
}

// What cannot be sent is said before it is sent, and in words.
func TestFilesThatCannotBeSent(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{MaxFile: 64 * 1024})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	dir := t.TempDir()
	write := func(name string, n int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, n), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	r := a.run(t, "send", room, "--file", write("big.bin", 64*1024+1))
	if r.code != 1 || !strings.Contains(r.stderr, "is 64.0 KiB; this server takes files up to 64.0 KiB") {
		t.Fatalf("a file over the cap: exit %d, %s", r.code, r.stderr)
	}
	a.ok(t, "send", room, "--file", write("fits.bin", 64*1024))

	if r := a.run(t, "send", room, "--file", write("empty.bin", 0)); r.code != 1 || !strings.Contains(r.stderr, "is empty") {
		t.Fatalf("an empty file: exit %d, %s", r.code, r.stderr)
	}
	if r := a.run(t, "send", room, "--file", dir); r.code != 1 || !strings.Contains(r.stderr, "not a regular file") {
		t.Fatalf("a directory: exit %d, %s", r.code, r.stderr)
	}
	if r := a.run(t, "send", room, "--file", filepath.Join(dir, "nope")); r.code != 1 || !strings.Contains(r.stderr, "no such file") {
		t.Fatalf("a missing file: exit %d, %s", r.code, r.stderr)
	}

	// A server with files switched off.
	off := startServer(t, server.Config{MaxFile: -1})
	b := newHome(t, off)
	b.ok(t, "join", room, "--name", "w", "--task", "t")
	if r := b.run(t, "send", room, "--file", write("any.bin", 10)); r.code != 1 || !strings.Contains(r.stderr, "takes no files") {
		t.Fatalf("send --file to a server with files off: exit %d, %s", r.code, r.stderr)
	}
}

// Files are part of what a restart loses, and the client says so plainly.
func TestAFileDoesNotOutliveTheServersMemory(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	src := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(src, []byte("kept in memory only\n"), 0o600)
	id := frames(t, a.ok(t, "send", room, "--file", src).stdout)[0]["file"].(map[string]any)["id"].(string)
	a.ok(t, "fetch", room, id)

	ts.restart()
	if r := a.run(t, "fetch", room, id); r.code != 1 || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("fetch after a restart: exit %d, %s", r.code, r.stderr)
	}
	// The sender's session recovers by itself, as for any send.
	r := a.ok(t, "send", room, "--file", src, "again")
	if !strings.Contains(r.stderr, "rejoined") {
		t.Fatalf("send --file after a restart should rejoin and say so: %s", r.stderr)
	}
}

// A fetch clears away what earlier fetches left more than a week ago, and
// nothing else.
func TestOldFetchedFilesAreClearedAway(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	src := filepath.Join(t.TempDir(), "n.txt")
	os.WriteFile(src, []byte("x\n"), 0o600)
	id := frames(t, a.ok(t, "send", room, "--file", src).stdout)[0]["file"].(map[string]any)["id"].(string)

	files := filepath.Join(a.dir, "files")
	os.MkdirAll(filepath.Join(files, "a-folder"), 0o700)
	old, recent := filepath.Join(files, "old"), filepath.Join(files, "recent")
	os.WriteFile(old, []byte("o"), 0o600)
	os.WriteFile(recent, []byte("r"), 0o600)
	longAgo := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(old, longAgo, longAgo)
	os.Chtimes(filepath.Join(files, "a-folder"), longAgo, longAgo)

	a.ok(t, "fetch", room, id)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a file fetched eight days ago is still there")
	}
	for _, keep := range []string{recent, filepath.Join(files, "a-folder")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
}

// A server from before files answers the new routes with a bare 404; the
// client turns that into words.
func TestAServerFromBeforeFiles(t *testing.T) {
	t.Parallel()
	real := server.NewAPI(server.NewHub(server.Config{}), testToken).Handler()
	old := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/files"):
			http.NotFound(w, r)
		case r.URL.Path == "/version":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"0.1.0","proto":"1"}`))
		default:
			real.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(old.Close)
	addr, port, _ := strings.Cut(old.Listener.Addr().String(), ":")
	h := home{t.TempDir()}
	h.ok(t, "register", addr, port, "hub", "--token", testToken)
	h.ok(t, "join", room, "--name", "w", "--task", "t")

	src := filepath.Join(t.TempDir(), "n.txt")
	os.WriteFile(src, []byte("x\n"), 0o600)
	if r := h.run(t, "send", room, "--file", src); r.code != 1 || !strings.Contains(r.stderr, "takes no files") {
		t.Fatalf("send --file to an old server: exit %d, %s", r.code, r.stderr)
	}
	if r := h.run(t, "fetch", room, "01ANY"); r.code != 1 || !strings.Contains(r.stderr, "older than file sharing") {
		t.Fatalf("fetch from an old server: exit %d, %s", r.code, r.stderr)
	}
}
