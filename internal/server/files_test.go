package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// pngBytes is the start of a PNG, padded to n bytes: enough for the server to
// recognise what it is.
func pngBytes(n int) []byte {
	b := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, n)...)
	return b[:n]
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

// A file rides on a message: the room sees one event, with the caption as
// its body, its mentions resolved, and a description of the file that the
// server wrote — the type from the bytes, not from anyone's say-so.
func TestAFileIsAMessage(t *testing.T) {
	h, _ := newTestHub(Config{})
	a := mustJoin(t, h, "r", agentID("zeus", "worker"))
	b := mustJoin(t, h, "r", agentID("anubis", "oracle"))

	data := pngBytes(4096)
	ev, unresolved, err := h.SendFile("r", a.PID, "@oracle the screen, and @nobody", "shot.png", data)
	if err != nil {
		t.Fatalf("send file: %v", err)
	}
	if ev.Type != proto.TypeMessage || ev.Kind != proto.KindText || ev.Body != "@oracle the screen, and @nobody" {
		t.Fatalf("the event is not a text message with the caption: %+v", ev)
	}
	if ev.From == nil || ev.From.Hostname != "zeus" || ev.From.Name != "worker" {
		t.Fatalf("from is not the sender: %+v", ev.From)
	}
	if len(ev.Mentions) != 1 || ev.Mentions[0].Name != "oracle" || len(unresolved) != 1 || unresolved[0] != "@nobody" {
		t.Fatalf("mentions %+v unresolved %v", ev.Mentions, unresolved)
	}
	f := ev.File
	if f == nil || f.ID == "" || f.Name != "shot.png" || f.Size != 4096 || f.Type != "image/png" {
		t.Fatalf("file description: %+v", f)
	}

	// The other participant gets it as an ordinary event, and the bytes by id.
	evs, err := h.EventsAfter("r", b.PID, b.Cursor)
	if err != nil || len(evs) != 1 || evs[0].File == nil || evs[0].File.ID != f.ID {
		t.Fatalf("the other side's events: %+v err %v", evs, err)
	}
	meta, got, err := h.File("r", f.ID)
	if err != nil || !bytes.Equal(got, data) || meta != *f {
		t.Fatalf("fetch: meta %+v err %v, bytes equal %v", meta, err, bytes.Equal(got, data))
	}

	// A caption is optional; a file is not.
	if _, _, err := h.SendFile("r", a.PID, "", "notes.txt", []byte("plain words\n")); err != nil {
		t.Fatalf("a file with no caption: %v", err)
	}
	_, _, err = h.SendFile("r", a.PID, "nothing", "empty", nil)
	wantCode(t, err, proto.ErrBadRequest)

	// Another room's id, or no such id: not found.
	_, _, err = h.File("r", "01NOSUCHFILE")
	wantCode(t, err, proto.ErrNotFound)
	_, _, err = h.File("elsewhere", f.ID)
	wantCode(t, err, proto.ErrNotFound)

	// A dead pid cannot send one.
	if err := h.Leave("r", a.PID); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.SendFile("r", a.PID, "", "late.txt", []byte("x"))
	wantCode(t, err, proto.ErrPIDGone)
}

// The caps: a file over the first is refused; past the other two the oldest
// files make way — the room's own first, then the oldest anywhere.
func TestFileCaps(t *testing.T) {
	h, _ := newTestHub(Config{MaxFile: 100, RoomFiles: 250, TotalFiles: 400, Burst: 100})
	send := func(room, pid string, n int) string {
		t.Helper()
		ev, _, err := h.SendFile(room, pid, "", "f.bin", bytes.Repeat([]byte{1}, n))
		if err != nil {
			t.Fatalf("send %d bytes to %s: %v", n, room, err)
		}
		return ev.File.ID
	}
	gone := func(room, id string) bool {
		_, _, err := h.File(room, id)
		return err != nil
	}

	a := mustJoin(t, h, "a", agentID("zeus", "w"))
	_, _, err := h.SendFile("a", a.PID, "", "big.bin", make([]byte, 101))
	wantCode(t, err, proto.ErrTooLarge)

	a1, a2, a3 := send("a", a.PID, 100), send("a", a.PID, 100), send("a", a.PID, 100)
	if !gone("a", a1) || gone("a", a2) || gone("a", a3) {
		t.Fatalf("room cap: expected only the first of three to be gone (%v %v %v)", gone("a", a1), gone("a", a2), gone("a", a3))
	}
	if n := h.FileBytes(); n != 200 {
		t.Fatalf("after the room cap the server holds %d bytes, want 200", n)
	}
	// The message that carried the evicted file is still in the history.
	evs, _ := h.History("a", "", 0)
	found := false
	for _, ev := range evs {
		found = found || (ev.File != nil && ev.File.ID == a1)
	}
	if !found {
		t.Fatal("the message whose file was evicted left the history too")
	}

	// Two more rooms fill the server to its cap exactly: nothing goes.
	b := mustJoin(t, h, "b", agentID("zeus", "w"))
	c := mustJoin(t, h, "c", agentID("zeus", "w"))
	b1, c1 := send("b", b.PID, 100), send("c", c.PID, 100)
	if gone("a", a2) || gone("b", b1) || gone("c", c1) || h.FileBytes() != 400 {
		t.Fatalf("at the cap exactly something went, or the count is off: %d bytes", h.FileBytes())
	}
	// One more, in a room with space of its own: the oldest file on the
	// server goes, and that is in another room.
	c2 := send("c", c.PID, 100)
	if !gone("a", a2) || gone("a", a3) || gone("b", b1) || gone("c", c1) || gone("c", c2) {
		t.Fatal("server cap: expected exactly the oldest file anywhere (a's second) to go")
	}
	if n := h.FileBytes(); n != 400 {
		t.Fatalf("after the server cap it holds %d bytes, want 400", n)
	}
}

// A file lives no longer than its message, and no longer than its room.
func TestFilesGoWithTheirMessageAndTheirRoom(t *testing.T) {
	h, clock := newTestHub(Config{RoomCap: 5, Burst: 100})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))                     // event 1
	ev, _, err := h.SendFile("r", a.PID, "", "f.txt", []byte("hello")) // event 2
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // events 3-5: the room is at its cap, nothing evicted
		if _, _, err := h.Send("r", a.PID, "filler", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.File("r", ev.File.ID); err != nil {
		t.Fatalf("the file went before its message did: %v", err)
	}
	for i := 0; i < 2; i++ { // two more: the join, then the file's message, fall off
		if _, _, err := h.Send("r", a.PID, "filler", ""); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = h.File("r", ev.File.ID)
	wantCode(t, err, proto.ErrNotFound)
	if n := h.FileBytes(); n != 0 {
		t.Fatalf("the evicted message's file is still counted: %d bytes", n)
	}

	// The idle sweep takes a room's files with it — at once, not a sweep later.
	b := mustJoin(t, h, "idle", agentID("zeus", "w"))
	ev, _, err = h.SendFile("idle", b.PID, "", "f.txt", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(9 * time.Hour)
	h.Sweep()
	if n := h.FileBytes(); n != 0 {
		t.Fatalf("an expired room's files are still held: %d bytes", n)
	}
	_, _, err = h.File("idle", ev.File.ID)
	wantCode(t, err, proto.ErrNotFound)
	// And a room of the same name afterwards starts with none of them.
	b = mustJoin(t, h, "idle", agentID("zeus", "w"))
	_, _, err = h.File("idle", ev.File.ID)
	wantCode(t, err, proto.ErrNotFound)
}

func TestNoFilesWhenOff(t *testing.T) {
	h, _ := newTestHub(Config{MaxFile: -1})
	a := mustJoin(t, h, "r", agentID("zeus", "w"))
	if h.MaxFile() != 0 {
		t.Fatalf("MaxFile() = %d on a server that takes none", h.MaxFile())
	}
	_, _, err := h.SendFile("r", a.PID, "", "f.txt", []byte("x"))
	wantCode(t, err, proto.ErrNotFound)
}

func TestFileNamesAreCleaned(t *testing.T) {
	long := strings.Repeat("é", 150) + ".png" // 304 bytes
	for in, want := range map[string]string{
		"shot.png":                "shot.png",
		"/etc/passwd":             "passwd",
		`C:\Users\x\shot.png`:     "shot.png",
		"../../.ssh/authorized":   "authorized",
		"..":                      "file",
		".":                       "file",
		"":                        "file",
		"   ":                     "file",
		".env":                    ".env",
		"a\nb\r.txt":              "ab.txt",
		"evil\u202egnp.exe":       "evilgnp.exe", // right-to-left override
		"tab\there.txt":           "tabhere.txt",
		"bad\xff\xfeutf8.txt":     "badutf8.txt",
		"naïve — résumé (v2).pdf": "naïve — résumé (v2).pdf",
	} {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
	got := cleanFileName(long)
	if len(got) > maxFileName || !strings.HasSuffix(got, ".png") || !strings.HasPrefix(got, "éé") || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("a long name was not cut to a valid one that keeps its extension: %d bytes, %q…", len(got), got[:20])
	}
}

// filePost sends a file the way a client does: who is sending, the name and
// the caption in the query, the bytes as the body. A nil meta leaves the pid
// out; nil data is an empty body.
func filePost(t *testing.T, endpoint, token string, meta any, name string, data []byte) (*http.Response, []byte) {
	t.Helper()
	q := url.Values{}
	if m, ok := meta.(proto.SendRequest); ok {
		q.Set("pid", m.PID)
		if m.Body != "" {
			q.Set("body", m.Body)
		}
	}
	if name != "" {
		q.Set("name", name)
	}
	return rawPost(t, endpoint+"?"+q.Encode(), token, "application/octet-stream", bytes.NewReader(data))
}

func rawPost(t *testing.T, endpoint, token, contentType string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the request failed outright — a refusal must be an answer the client can read: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func errCode(body []byte) string {
	var eb proto.ErrorBody
	json.Unmarshal(body, &eb)
	return eb.Error
}

func TestSendingAndFetchingAFileOverHTTP(t *testing.T) {
	ts, hub := testServer(t)
	const tok = "secret-token"
	_, pid, _ := join(t, ts.URL, tok, "zeus", "worker")

	// The cap is published where a client looks before it uploads.
	_, body := request(t, "GET", ts.URL+"/version", "", nil)
	var ver proto.VersionResponse
	json.Unmarshal(body, &ver)
	if ver.MaxFile != 25<<20 {
		t.Fatalf("/version max_file = %d, want the 25 MiB default", ver.MaxFile)
	}

	data := pngBytes(300 * 1024)
	name := "écran — 1.png"
	resp, body := filePost(t, ts.URL+"/api/rooms/ops/files", tok, proto.SendRequest{PID: pid, Body: "look, @worker"}, name, data)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	var sent proto.SendResponse
	json.Unmarshal(body, &sent)
	if sent.ID == "" || sent.File == nil || sent.File.Name != name || sent.File.Size != int64(len(data)) || sent.File.Type != "image/png" || len(sent.Mentions) != 1 {
		t.Fatalf("upload answer: %s", body)
	}

	// In the history, as part of its message.
	_, body = request(t, "GET", ts.URL+"/api/rooms/ops/history", tok, nil)
	var hist proto.EventsResponse
	json.Unmarshal(body, &hist)
	last := hist.Events[len(hist.Events)-1]
	if last.ID != sent.ID || last.Body != "look, @worker" || last.File == nil || *last.File != *sent.File {
		t.Fatalf("history's last event: %+v", last)
	}

	// The bytes: a download of unnamed type, whatever they are, with the name
	// and the found type beside it.
	resp, got := request(t, "GET", ts.URL+"/api/rooms/ops/files/"+sent.File.ID, tok, nil)
	if resp.StatusCode != 200 || !bytes.Equal(got, data) {
		t.Fatalf("download: %d, %d bytes (want %d)", resp.StatusCode, len(got), len(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q: an image from a room must not be served as one", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Crosstalk-File-Type") != "image/png" {
		t.Errorf("headers: %v", resp.Header)
	}
	disp, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || disp != "attachment" || params["filename"] != name {
		t.Errorf("Content-Disposition %q -> %q %v (%v)", resp.Header.Get("Content-Disposition"), disp, params, err)
	}

	// What is held is what is counted: exactly the file, with no room to spare.
	if f := hub.rooms["ops"].files[sent.File.ID]; cap(f.data) != len(data) || hub.FileBytes() != int64(len(data)) {
		t.Errorf("a %d-byte file is held in %d bytes and counted as %d", len(data), cap(f.data), hub.FileBytes())
	}

	// Without the token there is no file.
	resp, _ = request(t, "GET", ts.URL+"/api/rooms/ops/files/"+sent.File.ID, "wrong", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("download with a wrong token: %d", resp.StatusCode)
	}
	resp, body = request(t, "GET", ts.URL+"/api/rooms/ops/files/01NOSUCH", tok, nil)
	if resp.StatusCode != 404 || errCode(body) != proto.ErrNotFound {
		t.Fatalf("download of an unknown id: %d %s", resp.StatusCode, body)
	}
}

// Every refusal is an answer the client can read — also for a client that is
// already sending megabytes — and none of them keeps anything.
func TestFileRefusals(t *testing.T) {
	hub := NewHub(Config{MaxFile: 256 * 1024})
	api := NewAPI(hub, "secret-token")
	api.OperatorToken, api.OperatorHostname = "op-token", "hub"
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	const tok = "secret-token"
	endpoint := ts.URL + "/api/rooms/ops/files"

	_, pid, _ := join(t, ts.URL, tok, "zeus", "worker")
	_, opPID, _ := join(t, ts.URL, "op-token", "hub", "calper")
	big := bytes.Repeat([]byte{7}, 200*1024) // within the cap: large enough to still be on its way

	check := func(what string, resp *http.Response, body []byte, status int, code string) {
		t.Helper()
		if resp.StatusCode != status || errCode(body) != code {
			t.Errorf("%s: %d %s, want %d %s", what, resp.StatusCode, body, status, code)
		}
	}

	resp, body := filePost(t, endpoint, tok, proto.SendRequest{PID: "p_nobody"}, "f.bin", big)
	check("a pid the server does not know", resp, body, 410, proto.ErrPIDGone)
	resp, body = filePost(t, endpoint, tok, nil, "f.bin", big)
	check("no pid at all", resp, body, 410, proto.ErrPIDGone)

	// The operator's pid with the fleet's token: the file is not taken under
	// a signed-in person's name.
	resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: opPID}, "f.bin", big)
	check("the operator's pid with the fleet token", resp, body, 403, proto.ErrForbidden)
	resp, body = filePost(t, endpoint, "op-token", proto.SendRequest{PID: opPID}, "f.bin", big)
	if resp.StatusCode != 200 {
		t.Errorf("the operator's pid with the operator's token: %d %s", resp.StatusCode, body)
	}

	resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: pid}, "f.bin", bytes.Repeat([]byte{7}, 256*1024+1))
	check("one byte over the cap", resp, body, 413, proto.ErrTooLarge)
	resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: pid}, "f.bin", bytes.Repeat([]byte{7}, 256*1024))
	if resp.StatusCode != 200 {
		t.Errorf("exactly at the cap: %d %s", resp.StatusCode, body)
	}
	resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: pid, Body: strings.Repeat("c", 16*1024+1)}, "f.bin", big)
	check("a caption over its cap", resp, body, 413, proto.ErrTooLarge)
	resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: pid}, "f.bin", nil)
	check("an empty file", resp, body, 400, proto.ErrBadRequest)
	// Without a length the server will not start: it reads what it was told
	// to expect, and nothing open-ended.
	resp, body = rawPost(t, endpoint+"?pid="+pid+"&name=f.bin", tok, "application/octet-stream", io.MultiReader(strings.NewReader("no length given")))
	check("a body with no Content-Length", resp, body, 411, proto.ErrBadRequest)
	resp, body = filePost(t, endpoint, "wrong", proto.SendRequest{PID: pid}, "f.bin", big)
	check("a wrong token", resp, body, 401, proto.ErrUnauthorized)

	// Rate limited: said before the file is read, and not counted as a send.
	for i := 0; i < 10; i++ {
		resp, body = filePost(t, endpoint, tok, proto.SendRequest{PID: pid}, "f.bin", []byte("x"))
		if resp.StatusCode == 429 {
			break
		}
	}
	check("past the rate", resp, body, 429, proto.ErrRateLimited)
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a rate-limit refusal of a file carries no Retry-After")
	}

	// Two files were accepted with megabytes refused around them, and only
	// what was accepted is held.
	want := int64(200*1024 + 256*1024)
	if n := hub.FileBytes(); n < want || n > want+10 {
		t.Errorf("the server holds %d bytes of files; only the accepted ones (%d and a few one-byte files) should be there", n, want)
	}

	// A server that takes no files says so.
	off := NewAPI(NewHub(Config{MaxFile: -1}), tok)
	ts2 := httptest.NewServer(off.Handler())
	t.Cleanup(ts2.Close)
	_, p2, _ := join(t, ts2.URL, tok, "zeus", "worker")
	resp, body = filePost(t, ts2.URL+"/api/rooms/ops/files", tok, proto.SendRequest{PID: p2}, "f.bin", []byte("x"))
	check("a server with files off", resp, body, 404, proto.ErrNotFound)
	_, body = request(t, "GET", ts2.URL+"/version", "", nil)
	if strings.Contains(string(body), "max_file") {
		t.Errorf("/version of a server with files off still names a cap: %s", body)
	}
}

// What the server holds for a file is the file, and what it counts is what
// it holds — whatever the sender announced, and however the bytes arrived.
func TestAFileIsHeldAsWhatItIs(t *testing.T) {
	ts, hub := testServer(t)
	const tok = "secret-token"
	_, pid, _ := join(t, ts.URL, tok, "zeus", "worker")

	// Announce a large file and send one byte of it: there is no file.
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /api/rooms/ops/files?pid=%s&name=f.bin HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nX-Crosstalk-Proto: 1\r\nContent-Length: %d\r\n\r\nx",
		pid, tok, 20<<20)
	conn.(*net.TCPConn).CloseWrite()
	answer, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(answer), " 400 ") || hub.FileBytes() != 0 {
		t.Fatalf("a file announced as 20 MiB of which one byte came: %d bytes held, answer %.60q", hub.FileBytes(), answer)
	}
	if evs, _ := hub.History("ops", "", 0); len(evs) != 1 {
		t.Fatalf("a file that did not arrive left %d events in the room, want the join alone", len(evs))
	}

	// A caller handing the hub a slice with room to spare: the room is not kept.
	a := mustJoin(t, hub, "direct", agentID("zeus", "w"))
	roomy := make([]byte, 10, 1<<20)
	ev, _, err := hub.SendFile("direct", a.PID, "", "small.bin", roomy)
	if err != nil {
		t.Fatal(err)
	}
	if f := hub.rooms["direct"].files[ev.File.ID]; cap(f.data) != 10 || hub.FileBytes() != 10 {
		t.Fatalf("a 10-byte file from a 1 MiB slice is held in %d bytes and counted as %d", cap(f.data), hub.FileBytes())
	}

	// A name as long as the sender likes costs the server a name.
	long := strings.Repeat("a/", 200_000) + "real.txt"
	ev, _, err = hub.SendFile("direct", a.PID, "", long, []byte("x"))
	if err != nil || ev.File.Name != "real.txt" {
		t.Fatalf("a file under a 400 kB path: %+v err %v", ev.File, err)
	}
}

// The gate: so many at once, so many of those from one address, and a wait
// that ends.
func TestGate(t *testing.T) {
	g := newGate(3, 2)
	ctx := context.Background()
	leaveA1, ok1 := g.enter(ctx, "a", time.Second)
	_, ok2 := g.enter(ctx, "a", time.Second)
	if !ok1 || !ok2 {
		t.Fatal("two from one address were not let in")
	}
	if _, ok := g.enter(ctx, "a", 30*time.Millisecond); ok {
		t.Fatal("a third from the same address got in")
	}
	leaveB, ok := g.enter(ctx, "b", time.Second)
	if !ok {
		t.Fatal("another address was kept out while there was room")
	}
	if _, ok := g.enter(ctx, "c", 30*time.Millisecond); ok {
		t.Fatal("a fourth got in past the total")
	}
	// A slot given back lets the one that is waiting in.
	got := make(chan bool, 1)
	go func() {
		_, ok := g.enter(ctx, "c", 5*time.Second)
		got <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	leaveB()
	if !<-got {
		t.Fatal("a waiter was not let in when a slot came back")
	}
	// A request that ends stops waiting.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := g.enter(gone, "d", 5*time.Second); ok {
		t.Fatal("a request that had ended was let in")
	}
	leaveA1()
	if _, ok := g.enter(ctx, "a", time.Second); !ok {
		t.Fatal("the address's own slot did not come back")
	}
	// No gate at all admits everything.
	var none *gate
	if leave, ok := none.enter(ctx, "x", 0); !ok {
		t.Fatal("a nil gate refused")
	} else {
		leave()
	}
}

// With every slot taken, a file is told to come back — in the words the
// clients already wait on — and nothing of it is kept or held.
func TestBusyWithOtherFiles(t *testing.T) {
	defer func(d time.Duration) { slotWait = d }(slotWait)
	slotWait = 50 * time.Millisecond

	hub := NewHub(Config{})
	api := NewAPI(hub, "secret-token")
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	const tok = "secret-token"
	_, pid, _ := join(t, ts.URL, tok, "zeus", "worker")
	resp, body := filePost(t, ts.URL+"/api/rooms/ops/files", tok, proto.SendRequest{PID: pid}, "kept.bin", []byte("kept"))
	var sent proto.SendResponse
	json.Unmarshal(body, &sent)
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}

	var leave []func()
	for i := 0; i < maxUploads; i++ {
		l, ok := api.uploads.enter(context.Background(), fmt.Sprint("other-", i), time.Second)
		if !ok {
			t.Fatal("could not fill the upload slots")
		}
		leave = append(leave, l)
	}
	resp, body = filePost(t, ts.URL+"/api/rooms/ops/files", tok, proto.SendRequest{PID: pid}, "f.bin", bytes.Repeat([]byte{1}, 100*1024))
	if resp.StatusCode != 429 || errCode(body) != proto.ErrRateLimited || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("an upload with every slot taken: %d %s (Retry-After %q)", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if hub.FileBytes() != 4 {
		t.Fatalf("a refused upload changed what is held: %d bytes", hub.FileBytes())
	}
	for _, l := range leave {
		l()
	}
	if resp, body = filePost(t, ts.URL+"/api/rooms/ops/files", tok, proto.SendRequest{PID: pid}, "f.bin", []byte("now")); resp.StatusCode != 200 {
		t.Fatalf("an upload once the slots are free: %d %s", resp.StatusCode, body)
	}

	// The same for downloads.
	leave = nil
	for i := 0; i < maxDownloads; i++ {
		l, ok := api.downloads.enter(context.Background(), fmt.Sprint("other-", i), time.Second)
		if !ok {
			t.Fatal("could not fill the download slots")
		}
		leave = append(leave, l)
	}
	resp, body = request(t, "GET", ts.URL+"/api/rooms/ops/files/"+sent.File.ID, tok, nil)
	if resp.StatusCode != 429 || errCode(body) != proto.ErrRateLimited {
		t.Fatalf("a download with every slot taken: %d %s", resp.StatusCode, body)
	}
	for _, l := range leave {
		l()
	}
	if resp, body = request(t, "GET", ts.URL+"/api/rooms/ops/files/"+sent.File.ID, tok, nil); resp.StatusCode != 200 || string(body) != "kept" {
		t.Fatalf("a download once the slots are free: %d %q", resp.StatusCode, body)
	}
}

// A small file may not hold a slot for long; a large one gets the time it
// needs on a slow link, and no transfer more than the bound.
func TestTransferTime(t *testing.T) {
	for size, want := range map[int64]time.Duration{
		1:         30 * time.Second,
		64 * 1024: 31 * time.Second,
		1 << 20:   46 * time.Second,
		25 << 20:  430 * time.Second,
		1 << 30:   fileTransferMax,
	} {
		if got := transferTime(size); got != want {
			t.Errorf("transferTime(%d) = %v, want %v", size, got, want)
		}
	}
}
