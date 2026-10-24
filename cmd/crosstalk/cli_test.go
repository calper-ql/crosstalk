package main

// End-to-end tests of the agent contract: the real binary, run as an agent
// would run it, against an in-process server. Exit codes, stdout shape, and
// timing are what agents depend on, so that is what these check.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/server"
)

var bin string // the crosstalk binary under test

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crosstalk-cli-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "crosstalk")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building crosstalk: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const testToken = "test-token"

// testServer is an in-process crosstalkd over TLS whose state can be wiped —
// a simulated restart — without changing its address or certificate.
type testServer struct {
	addr, port string
	cfg        server.Config
	hub        atomic.Pointer[server.Hub]
	handler    atomic.Pointer[http.Handler]
}

func startServer(t *testing.T, cfg server.Config) *testServer {
	t.Helper()
	ts := &testServer{cfg: cfg}
	ts.restart()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*ts.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	ts.addr, ts.port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return ts
}

func (ts *testServer) restart() {
	hub := server.NewHub(ts.cfg)
	h := server.NewAPI(hub, testToken).Handler()
	ts.hub.Store(hub)
	ts.handler.Store(&h)
}

// home is one client machine's ~/.crosstalk, registered with a server.
type home struct{ dir string }

func newHome(t *testing.T, ts *testServer) home {
	t.Helper()
	h := home{t.TempDir()}
	h.ok(t, "register", ts.addr, ts.port, "hub", "--token", testToken)
	return h
}

type result struct {
	stdout, stderr string
	code           int
}

func (h home) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CROSSTALK_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "CROSSTALK_HOME="+h.dir)
	return cmd
}

func (h home) run(t *testing.T, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := h.cmd(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	return finish(t, cmd.Run(), &out, &errb)
}

func finish(t *testing.T, err error, out, errb *bytes.Buffer) result {
	t.Helper()
	r := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		t.Fatalf("running crosstalk: %v", err)
	}
	return r
}

// ok runs a command that must succeed.
func (h home) ok(t *testing.T, args ...string) result {
	t.Helper()
	r := h.run(t, args...)
	if r.code != 0 {
		t.Fatalf("crosstalk %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// frames decodes NDJSON stdout.
func frames(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func hostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const room = "ai#42"

func TestWaitTimeoutIsHonoured(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")

	start := time.Now()
	r := a.run(t, "wait", room, "--timeout", "2")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("wait --timeout 2 took %v", took)
	}
	if r.code != 2 || r.stdout != "" {
		t.Fatalf("expected exit 2 with empty stdout, got %d %q", r.code, r.stdout)
	}
}

func TestConversationBetweenAgents(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	host := hostname(t)
	a, b := newHome(t, ts), newHome(t, ts)
	a.ok(t, "join", room, "--name", "worker-3", "--task", "porting")

	// The join line says who you are and who else is here.
	j := frames(t, b.ok(t, "join", room, "--name", "oracle", "--task", "reviewing").stdout)[0]
	if j["you"] != host+"/oracle" || fmt.Sprint(j["present"]) != "["+host+"/worker-3]" {
		t.Fatalf("unexpected join report: %v", j)
	}

	// A mention reports who it notified; the addressee's wait marks it.
	s := frames(t, a.ok(t, "send", room, "@oracle layer 14 ready").stdout)[0]
	if fmt.Sprint(s["mentions"]) != fmt.Sprintf("[map[hostname:%s name:oracle]]", host) {
		t.Fatalf("send should echo resolved mentions: %v", s)
	}
	ev := frames(t, b.ok(t, "wait", room, "--mentions-only", "--timeout", "10").stdout)[0]
	if ev["to_you"] != true || ev["body"] != "@oracle layer 14 ready" {
		t.Fatalf("expected the mention, marked to_you: %v", ev)
	}

	// A mention that matches no one still sends, but says so loudly.
	r := a.ok(t, "send", room, "@orcale did you see it?")
	if !strings.Contains(r.stderr, "@orcale matched no one") {
		t.Fatalf("expected an unresolved-mention warning, got stderr %q", r.stderr)
	}

	// Text format: one compact line, marked as addressed.
	a.ok(t, "send", room, "@oracle ping")
	line := strings.TrimSpace(b.ok(t, "wait", room, "--timeout", "10", "--format", "text").stdout)
	want := regexp.MustCompile(`\[\d\d:\d\d:\d\d\] ` + regexp.QuoteMeta(host+"/worker-3") + ` → you: @oracle ping$`)
	if !want.MatchString(line) {
		t.Fatalf("unexpected text output:\n%s", line)
	}

	// who lists everyone with what to type, and marks you.
	who := frames(t, b.ok(t, "who", room).stdout)
	if len(who) != 2 || who[1]["mention"] != "@"+host+"/oracle" || who[1]["you"] != true {
		t.Fatalf("unexpected who output: %v", who)
	}
}

func TestFlagsAnywhereAndDashBodies(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", "--name", "w", room, "--task", "t")
	a.ok(t, "send", "--kind", "status", room, "layer", "14", "done")
	a.ok(t, "send", room, "-1 offset fixed")
	a.ok(t, "send", room, "--", "-v")
	if r := a.run(t, "send", room, "hi", "--knd", "status"); r.code != 1 || !strings.Contains(r.stderr, "unknown flag --knd") {
		t.Fatalf("a misspelt flag must fail, not join the body: %+v", r)
	}

	var got []string
	for _, ev := range frames(t, a.ok(t, "history", room, "--messages-only").stdout) {
		got = append(got, fmt.Sprintf("%s:%s", ev["kind"], ev["body"]))
	}
	if want := "[status:layer 14 done text:-1 offset fixed text:-v]"; fmt.Sprint(got) != want {
		t.Fatalf("history = %v, want %s", got, want)
	}
}

func TestBacklogOnJoin(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a, b := newHome(t, ts), newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	for _, m := range []string{"one", "two", "three"} {
		a.ok(t, "send", room, m)
	}

	// Without --backlog, a late joiner is pointed at the history.
	r := b.ok(t, "join", room, "--name", "late", "--task", "t")
	if len(frames(t, r.stdout)) != 1 || !strings.Contains(r.stderr, "already has a conversation") {
		t.Fatalf("expected a catch-up hint: %+v", r)
	}
	out := frames(t, b.ok(t, "join", room, "--name", "later", "--task", "t", "--backlog", "2").stdout)
	if len(out) != 3 || out[1]["body"] != "two" || out[2]["body"] != "three" {
		t.Fatalf("expected join line + last 2 messages: %v", out)
	}
}

func TestAsTakesRoleName(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "worker-3", "--task", "t")
	r := a.ok(t, "join", room, "--name", "worker-4", "--task", "t")
	if !strings.Contains(r.stderr, "--as worker-4") {
		t.Fatalf("second local session should be told to use --as: %q", r.stderr)
	}
	if r := a.run(t, "send", room, "hi"); r.code != 1 || !strings.Contains(r.stderr, "--as <name>") {
		t.Fatalf("ambiguous send should fail pointing at --as: %+v", r)
	}
	a.ok(t, "send", room, "hi", "--as", "worker-4")
	ev := frames(t, a.ok(t, "history", room, "--messages-only", "--as", "worker-3").stdout)[0]
	if ev["from"].(map[string]any)["name"] != "worker-4" || ev["from_you"] == true {
		t.Fatalf("expected worker-4's message, not marked as worker-3's: %v", ev)
	}
}

func TestRejoinsAfterServerRestart(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "first")
	a.ok(t, "task", room, "--set", "second")

	ts.restart()
	r := a.ok(t, "send", room, "still here")
	if !strings.Contains(r.stderr, "rejoined") {
		t.Fatalf("expected a rejoin note: %q", r.stderr)
	}
	who := frames(t, a.ok(t, "who", room).stdout)
	if len(who) != 1 || who[0]["task"] != "second" {
		t.Fatalf("rejoin should redeclare the latest task: %v", who)
	}

	ts.restart()
	if r := a.run(t, "wait", room, "--timeout", "2"); r.code != 2 || !strings.Contains(r.stderr, "rejoined") {
		t.Fatalf("wait should rejoin and then time out normally: %+v", r)
	}
}

func TestRateLimitIsWaitedOut(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{Burst: 1, RatePerMin: 60})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	a.ok(t, "send", room, "one")
	if r := a.ok(t, "send", room, "two"); !strings.Contains(r.stderr, "rate limited — sending in") {
		t.Fatalf("expected the second send to be paced: %q", r.stderr)
	}
}

func TestRoomClosedWakesWait(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{IdleExpiry: time.Millisecond})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := a.cmd(ctx, "wait", room, "--mentions-only")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let the poll park
	ts.hub.Load().Sweep()
	r := finish(t, cmd.Wait(), &out, &errb)
	if r.code != 0 || len(frames(t, r.stdout)) != 1 || frames(t, r.stdout)[0]["type"] != "room_closed" {
		t.Fatalf("expected room_closed and exit 0: %+v", r)
	}
}

func TestSessionsPrune(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	a := newHome(t, ts)
	a.ok(t, "join", room, "--name", "w", "--task", "t")
	ts.restart()
	s := frames(t, a.ok(t, "sessions", "--prune").stdout)
	if len(s) != 1 || s[0]["status"] != "gone" {
		t.Fatalf("expected the dead session reported gone: %v", s)
	}
	if out := a.ok(t, "sessions").stdout; out != "" {
		t.Fatalf("pruned session still listed: %s", out)
	}
}
