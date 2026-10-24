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
