package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// lockServer is a server with an operator and a place to keep its lock.
func lockServer(t *testing.T) (*httptest.Server, *API, string) {
	t.Helper()
	hub := NewHub(Config{})
	api := NewAPI(hub, "fleet-token")
	api.OperatorToken, api.OperatorHostname = "operator-token", "hub"
	api.LockPath = filepath.Join(t.TempDir(), LockFile)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return ts, api, api.LockPath
}

func joinAs(t *testing.T, ts *httptest.Server, token, room, host string) proto.JoinResponse {
	t.Helper()
	resp, body := request(t, "POST", ts.URL+"/api/rooms/"+room+"/join", token,
		proto.JoinRequest{Hostname: host, Name: "a", Mode: "agent", Task: "testing"})
	if resp.StatusCode != 200 {
		t.Fatalf("join: %d %s", resp.StatusCode, body)
	}
	var jr proto.JoinResponse
	json.Unmarshal(body, &jr)
	return jr
}

func wantLocked(t *testing.T, what string, resp *http.Response, body []byte) {
	t.Helper()
	var eb proto.ErrorBody
	json.Unmarshal(body, &eb)
	if resp.StatusCode != http.StatusServiceUnavailable || eb.Error != proto.ErrLockedDown {
		t.Fatalf("%s: want 503 %s, got %d %s", what, proto.ErrLockedDown, resp.StatusCode, body)
	}
}

func TestOnlyTheOperatorCanLockTheServerDown(t *testing.T) {
	ts, api, path := lockServer(t)
	joinAs(t, ts, "fleet-token", "r", "zeus")

	resp, body := request(t, "POST", ts.URL+"/api/lockdown", "fleet-token", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the fleet token locked the server down: %d %s", resp.StatusCode, body)
	}
	if resp, body := request(t, "POST", ts.URL+"/api/lockdown", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", resp.StatusCode, body)
	}
	if api.Locked() {
		t.Fatal("locked after a refused request")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a lock file was written for a refused request")
	}
	if len(api.Hub.Rooms()) != 1 {
		t.Fatal("the room is gone after a refused request")
	}
}

func TestALockedServerAnswersNothing(t *testing.T) {
	ts, api, path := lockServer(t)
	jr := joinAs(t, ts, "fleet-token", "r", "zeus")
	if resp, body := request(t, "POST", ts.URL+"/api/rooms/r/messages", "fleet-token",
		proto.SendRequest{PID: jr.PID, Body: "a thing said before"}); resp.StatusCode != 200 {
		t.Fatalf("send: %d %s", resp.StatusCode, body)
	}
	if _, _, err := api.Hub.SendFile("r", jr.PID, "", "shot.png", pngBytes(4096)); err != nil {
		t.Fatalf("file: %v", err)
	}
	if api.Hub.FileBytes() == 0 {
		t.Fatal("the file was not kept")
	}

	resp, body := request(t, "POST", ts.URL+"/api/lockdown", "operator-token", nil)
	var lr proto.LockdownResponse
	json.Unmarshal(body, &lr)
	if resp.StatusCode != 200 || !lr.Locked || !lr.Kept {
		t.Fatalf("lockdown: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no lock file: %v", err)
	}
	if n := len(api.Hub.Rooms()); n != 0 {
		t.Fatalf("%d rooms are still held", n)
	}
	if n := api.Hub.FileBytes(); n != 0 {
		t.Fatalf("%d bytes of files are still held", n)
	}

	// Everything is refused, to both tokens, and with none.
	for _, token := range []string{"fleet-token", "operator-token", ""} {
		for _, c := range []struct{ method, path string }{
			{"POST", "/api/rooms/r/join"},
			{"GET", "/api/rooms"},
			{"GET", "/api/rooms/r/history"},
			{"GET", "/api/rooms/r/events?pid=" + jr.PID},
			{"POST", "/api/lockdown"},
			{"GET", "/install.sh"},
			{"GET", "/bin/crosstalk-linux-amd64"},
			{"GET", "/"},
		} {
			resp, body := request(t, c.method, ts.URL+c.path, token, proto.JoinRequest{Hostname: "zeus", Name: "a", Mode: "agent", Task: "testing"})
			wantLocked(t, c.method+" "+c.path+" with token "+token, resp, body)
		}
	}
	// But it says that it is locked, to whoever asks.
	resp, body = request(t, "GET", ts.URL+"/version", "", nil)
	var vr proto.VersionResponse
	json.Unmarshal(body, &vr)
	if resp.StatusCode != 200 || !vr.Locked {
		t.Fatalf("/version: %d %s", resp.StatusCode, body)
	}
	// And nothing is made behind the door either.
	if _, err := api.Hub.Join("r", proto.Identity{Hostname: "zeus", Name: "a", Mode: "agent", Task: "testing"}, "t"); err == nil {
		t.Fatal("a room was made in a locked server")
	}
}

func TestALockCutsWhatWasBeingServed(t *testing.T) {
	ts, _, _ := lockServer(t)
	jr := joinAs(t, ts, "fleet-token", "r", "zeus")

	// A poll that would wait thirty seconds for a message.
	type result struct {
		status int
		err    error
		took   time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		req, _ := http.NewRequest("GET", ts.URL+"/api/rooms/r/events?wait=30&pid="+jr.PID+"&after="+jr.Cursor, nil)
		req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
		req.Header.Set("Authorization", "Bearer fleet-token")
		// A connection of its own, never used before: Go's client sends a
		// request again by itself when a connection it had used before closes
		// under it, and the second sending would be refused at the door —
		// which is not the thing being looked at here.
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		r := result{err: err, took: time.Since(start)}
		if err == nil {
			r.status = resp.StatusCode
			resp.Body.Close()
		}
		done <- r
	}()
	time.Sleep(300 * time.Millisecond) // parked by now

	if resp, body := request(t, "POST", ts.URL+"/api/lockdown", "operator-token", nil); resp.StatusCode != 200 {
		t.Fatalf("lockdown: %d %s", resp.StatusCode, body)
	}
	select {
	case r := <-done:
		// Cut: the connection closed under it. It was not answered.
		if r.err == nil {
			t.Fatalf("the parked poll was answered (%d) by a server that had been locked down", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked poll was still waiting five seconds after the lock")
	}
}

func TestALockOutlastsARestart(t *testing.T) {
	_, api, path := lockServer(t)
	if _, err := api.Lock(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// The same state directory, a new process.
	again := NewAPI(NewHub(Config{}), "fleet-token")
	again.OperatorToken, again.OperatorHostname = "operator-token", "hub"
	again.LockPath = path
	if !again.LoadLock() {
		t.Fatal("started unlocked with the lock file there")
	}
	ts := httptest.NewServer(again.Handler())
	defer ts.Close()
	resp, body := request(t, "POST", ts.URL+"/api/rooms/r/join", "operator-token",
		proto.JoinRequest{Hostname: "hub", Name: "a", Mode: "human"})
	wantLocked(t, "join after a restart", resp, body)

	// Only taking the file away, and starting again, lifts it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fresh := NewAPI(NewHub(Config{}), "fleet-token")
	fresh.LockPath = path
	if fresh.LoadLock() {
		t.Fatal("started locked with no lock file")
	}
	ts2 := httptest.NewServer(fresh.Handler())
	defer ts2.Close()
	joinAs(t, ts2, "fleet-token", "r", "zeus")
}

func TestALockThatCannotBeWrittenStillLocks(t *testing.T) {
	ts, api, _ := lockServer(t)
	api.LockPath = filepath.Join(t.TempDir(), "no-such-folder", LockFile)
	resp, body := request(t, "POST", ts.URL+"/api/lockdown", "operator-token", nil)
	var lr proto.LockdownResponse
	json.Unmarshal(body, &lr)
	if resp.StatusCode != 200 || !lr.Locked || lr.Kept {
		t.Fatalf("lockdown: %d %s (want locked, and told it was not kept)", resp.StatusCode, body)
	}
	resp, body = request(t, "GET", ts.URL+"/api/rooms", "fleet-token", nil)
	wantLocked(t, "rooms", resp, body)
}
