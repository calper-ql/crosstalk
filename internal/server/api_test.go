package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/proto"
)

func testServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub(Config{})
	api := NewAPI(hub, "secret-token")
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return ts, hub
}

func request(t *testing.T, method, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Crosstalk-Proto", proto.ProtoVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp, out.Bytes()
}

func TestAuthAndProtoEnforcement(t *testing.T) {
	ts, _ := testServer(t)

	// Wrong token → 401 with the stable code.
	resp, body := request(t, "GET", ts.URL+"/api/rooms", "wrong", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, body)
	}
	var eb proto.ErrorBody
	json.Unmarshal(body, &eb)
	if eb.Error != proto.ErrUnauthorized {
		t.Fatalf("expected unauthorized code, got %+v", eb)
	}

	// Missing proto header → 426 proto_unsupported.
	req, _ := http.NewRequest("GET", ts.URL+"/api/rooms", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("expected 426, got %d", resp2.StatusCode)
	}

	// /version is unauthenticated.
	resp3, err := http.Get(ts.URL + "/version")
	if err != nil || resp3.StatusCode != 200 {
		t.Fatalf("version endpoint: %v %v", resp3.StatusCode, err)
	}
	resp3.Body.Close()
}

func TestRoomNameWithHashInPath(t *testing.T) {
	ts, _ := testServer(t)
	url := ts.URL + "/api/rooms/ai%2342/join"
	resp, body := request(t, "POST", url, "secret-token",
		proto.JoinRequest{Mode: "agent", Hostname: "zeus", Name: "w", Task: "t"})
	if resp.StatusCode != 200 {
		t.Fatalf("join via escaped room name: %d %s", resp.StatusCode, body)
	}
	var jr proto.JoinResponse
	json.Unmarshal(body, &jr)
	if jr.PID == "" {
		t.Fatalf("no pid in response: %s", body)
	}

	// The room shows up under its unescaped name.
	resp, body = request(t, "GET", ts.URL+"/api/rooms", "secret-token", nil)
	var rooms []proto.RoomInfo
	json.Unmarshal(body, &rooms)
	if len(rooms) != 1 || rooms[0].Name != "ai#42" {
		t.Fatalf("rooms list: %d %s", resp.StatusCode, body)
	}
}

func TestLongPollDelivery(t *testing.T) {
	ts, hub := testServer(t)
	join := func(name string) proto.JoinResponse {
		_, body := request(t, "POST", ts.URL+"/api/rooms/r/join", "secret-token",
			proto.JoinRequest{Mode: "agent", Hostname: "zeus", Name: name, Task: "t"})
		var jr proto.JoinResponse
		json.Unmarshal(body, &jr)
		return jr
	}
	a, b := join("a"), join("b")

	type result struct {
		events []proto.Event
		took   time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		// Poll from b's join cursor (the true room head) so the parked
		// poll isn't satisfied by b's join presence event.
		_, body := request(t, "GET",
			fmt.Sprintf("%s/api/rooms/r/events?pid=%s&after=%s&wait=10", ts.URL, a.PID, b.Cursor),
			"secret-token", nil)
		var er proto.EventsResponse
		json.Unmarshal(body, &er)
		done <- result{er.Events, time.Since(start)}
	}()

	time.Sleep(300 * time.Millisecond) // let the poll park
	if _, _, err := hub.Send("r", b.PID, "wake", ""); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.took > 5*time.Second {
			t.Fatalf("poll did not complete promptly on send (%v)", r.took)
		}
		if len(r.events) != 1 || r.events[0].Type != proto.TypeMessage || r.events[0].Body != "wake" {
			t.Fatalf("unexpected events: %+v", r.events)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("long-poll never completed")
	}
}

func TestLongPollTimeoutEmpty(t *testing.T) {
	ts, _ := testServer(t)
	_, body := request(t, "POST", ts.URL+"/api/rooms/r/join", "secret-token",
		proto.JoinRequest{Mode: "agent", Hostname: "zeus", Name: "a", Task: "t"})
	var jr proto.JoinResponse
	json.Unmarshal(body, &jr)

	start := time.Now()
	_, body = request(t, "GET",
		fmt.Sprintf("%s/api/rooms/r/events?pid=%s&after=%s&wait=1", ts.URL, jr.PID, jr.Cursor),
		"secret-token", nil)
	if took := time.Since(start); took < 900*time.Millisecond || took > 5*time.Second {
		t.Fatalf("wait=1 poll took %v", took)
	}
	var er proto.EventsResponse
	json.Unmarshal(body, &er)
	if len(er.Events) != 0 {
		t.Fatalf("expected empty timeout response, got %+v", er.Events)
	}
}

// operatorServer is a server with the operator configured: the hostname
// "hub" belongs to whoever holds "op-token".
func operatorServer(t *testing.T) *httptest.Server {
	t.Helper()
	api := NewAPI(NewHub(Config{}), "secret-token")
	api.OperatorToken, api.OperatorHostname = "op-token", "hub"
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func join(t *testing.T, base, token, hostname, name string) (int, string, string) {
	t.Helper()
	resp, body := request(t, "POST", base+"/api/rooms/ops/join", token,
		proto.JoinRequest{Mode: proto.ModeHuman, Hostname: hostname, Name: name})
	var jr proto.JoinResponse
	var eb proto.ErrorBody
	json.Unmarshal(body, &jr)
	json.Unmarshal(body, &eb)
	return resp.StatusCode, jr.PID, eb.Error
}

// The fleet token is shared by every machine, so it must not be enough to
// speak as the operator: not by declaring the operator's hostname, and not
// by using a pid the operator was given.
func TestOperatorHostnameAndPIDsAreTheOperators(t *testing.T) {
	ts := operatorServer(t)

	// The reserved hostname, in any case, with the fleet token: refused. And
	// so is one that only looks like it: a Cyrillic һ, fullwidth letters, or
	// any other hostname no machine has.
	for _, h := range []string{"hub", "Hub", "HUB", "һub", "ｈｕｂ", "hüb", "зевс"} {
		if status, _, code := join(t, ts.URL, "secret-token", h, "can"); status != 403 || code != proto.ErrForbidden {
			t.Errorf("fleet token joined as %q: %d %s", h, status, code)
		}
	}
	// With the operator token: accepted.
	status, opPID, _ := join(t, ts.URL, "op-token", "hub", "can")
	if status != 200 || opPID == "" {
		t.Fatalf("operator join: %d", status)
	}
	// Everything else is as it was, with either token.
	status, fleetPID, _ := join(t, ts.URL, "secret-token", "zeus", "worker")
	if status != 200 {
		t.Fatalf("fleet join under its own hostname: %d", status)
	}
	if status, _, _ := join(t, ts.URL, "op-token", "zeus", "someone"); status != 200 {
		t.Errorf("the operator token is not accepted where the fleet token is: %d", status)
	}

	// The operator's pid, presented with the fleet token: refused on every
	// route that takes a pid — and the message was not posted.
	calls := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/rooms/ops/messages", proto.SendRequest{PID: opPID, Body: "as the operator"}},
		{"POST", "/api/rooms/ops/task", proto.TaskRequest{PID: opPID, Task: "pretending"}},
		{"POST", "/api/rooms/ops/leave", proto.LeaveRequest{PID: opPID}},
		{"GET", "/api/rooms/ops/events?pid=" + opPID, nil},
	}
	for _, c := range calls {
		resp, body := request(t, c.method, ts.URL+c.path, "secret-token", c.body)
		var eb proto.ErrorBody
		json.Unmarshal(body, &eb)
		if resp.StatusCode != 403 || eb.Error != proto.ErrForbidden {
			t.Errorf("%s %s with the fleet token and the operator's pid: %d %s", c.method, c.path, resp.StatusCode, body)
		}
	}
	// The operator itself can use it, and an ordinary pid works with either token.
	if resp, body := request(t, "POST", ts.URL+"/api/rooms/ops/messages", "op-token", proto.SendRequest{PID: opPID, Body: "the real one"}); resp.StatusCode != 200 {
		t.Fatalf("operator send: %d %s", resp.StatusCode, body)
	}
	if resp, body := request(t, "POST", ts.URL+"/api/rooms/ops/messages", "op-token", proto.SendRequest{PID: fleetPID, Body: "from zeus"}); resp.StatusCode != 200 {
		t.Errorf("an ordinary pid with the operator token: %d %s", resp.StatusCode, body)
	}

	// What is in the room is exactly the two real messages, under their own names.
	_, body := request(t, "GET", ts.URL+"/api/rooms/ops/history", "secret-token", nil)
	var hist proto.EventsResponse
	json.Unmarshal(body, &hist)
	var said []string
	for _, ev := range hist.Events {
		if ev.Type == proto.TypeMessage {
			said = append(said, ev.From.Hostname+"/"+ev.From.Name+": "+ev.Body)
		}
	}
	if fmt.Sprint(said) != "[hub/can: the real one zeus/worker: from zeus]" {
		t.Errorf("messages in the room: %v", said)
	}
}

// With no operator configured nothing is reserved: a server run without the
// hub behaves exactly as before.
func TestNoOperatorNothingReserved(t *testing.T) {
	ts, _ := testServer(t)
	status, pid, _ := join(t, ts.URL, "secret-token", "hub", "can")
	if status != 200 {
		t.Fatalf("join as hub on a server with no operator: %d", status)
	}
	if resp, body := request(t, "POST", ts.URL+"/api/rooms/ops/messages", "secret-token", proto.SendRequest{PID: pid, Body: "hi"}); resp.StatusCode != 200 {
		t.Errorf("send: %d %s", resp.StatusCode, body)
	}
	// Nor is a hostname in another script: without an operator there is
	// nobody to imitate.
	if status, _, _ := join(t, ts.URL, "secret-token", "зевс", "worker"); status != 200 {
		t.Errorf("join under a non-ASCII hostname on a server with no operator: %d", status)
	}
	// And an empty operator token is not a token: a request with no bearer
	// at all must not match it.
	if resp, _ := request(t, "GET", ts.URL+"/api/rooms", "", nil); resp.StatusCode != 401 {
		t.Errorf("no token on a server with no operator: %d", resp.StatusCode)
	}
}
