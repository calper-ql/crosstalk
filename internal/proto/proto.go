// Package proto holds the wire types shared by crosstalkd and the crosstalk
// client. Events read from the server are the same JSON objects the CLI
// emits as NDJSON, so there is exactly one format everywhere.
package proto

import (
	"fmt"
	"time"
)

// ProtoVersion is sent in the X-Crosstalk-Proto header on every API request.
const ProtoVersion = "1"

// Version is the semantic version of both binaries. Build is the exact
// build identity (git describe), injected at link time by run.sh via
// -ldflags "-X crosstalk/internal/proto.Build=...". Two binaries are the
// same tool iff both fields match — `crosstalk update` compares both, so
// a rebuilt server pushes new clients even within one version number.
const Version = "0.1.0"

var Build = "dev"

// FullVersion renders "0.1.0 (build abc1234)".
func FullVersion() string {
	return Version + " (build " + Build + ")"
}

// DefaultPort is CROSS on a phone keypad.
const DefaultPort = 27677

// Modes a participant can join in.
const (
	ModeAgent = "agent"
	ModeHuman = "human"
)

// Message kinds.
const (
	KindText   = "text"
	KindStatus = "status"
	KindCode   = "code"
)

// Event types.
const (
	TypeMessage    = "message"
	TypePresence   = "presence"
	TypeRoomClosed = "room_closed"
)

// Presence sub-events.
const (
	PresenceJoin  = "join"
	PresenceLeave = "leave"
	PresenceTask  = "task"
)

// Identity is the composite participant identity. Hostname is stamped by the
// client binary, name is self-identification, task is the current activity.
type Identity struct {
	Hostname string `json:"hostname"`
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Task     string `json:"task,omitempty"`
}

// Display renders the canonical identity form: agents as hostname/name,
// humans as bare name.
func (id Identity) Display() string {
	if id.Mode == ModeHuman {
		return id.Name
	}
	return id.Hostname + "/" + id.Name
}

// Mention is one resolved entry of a message's mentions array. Either All is
// true (@room) or Hostname+Name identify a participant.
type Mention struct {
	Hostname string `json:"hostname,omitempty"`
	Name     string `json:"name,omitempty"`
	All      bool   `json:"all,omitempty"`
}

// File describes the file a message carries: its name, its size in bytes,
// and Type — what the server found in the bytes (a media type, as
// net/http's sniffing names it), never what the sender claimed. The bytes
// themselves are fetched separately, by ID, and are held in the server's
// memory only: they go when the room does, and earlier when newer files
// need the space.
type File struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

// HumanSize renders a byte count the way the limits are written: binary
// units, one decimal where it says something.
func HumanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1024*1024*1024))
}

// Event is a single room event. Fields are populated by type:
// message: From, Body, Kind, Mentions, and File when it carries one (the
// body is then its caption, and may be empty); presence: Event, Who;
// room_closed: Reason.
type Event struct {
	Type     string    `json:"type"`
	ID       string    `json:"id"`
	TS       time.Time `json:"ts"`
	From     *Identity `json:"from,omitempty"`
	Body     string    `json:"body,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Mentions []Mention `json:"mentions,omitempty"`
	File     *File     `json:"file,omitempty"`
	Event    string    `json:"event,omitempty"`
	Who      *Identity `json:"who,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Request and response bodies.

type JoinRequest struct {
	Mode     string `json:"mode"`
	Hostname string `json:"hostname"`
	Name     string `json:"name"`
	Task     string `json:"task,omitempty"`
}

// JoinResponse returns the minted pid and the room's current head cursor,
// so a joiner's first wait never fires on history from before it arrived.
type JoinResponse struct {
	PID    string `json:"pid"`
	Cursor string `json:"cursor"`
}

type SendRequest struct {
	PID  string `json:"pid"`
	Body string `json:"body"`
	Kind string `json:"kind,omitempty"`
}

// SendResponse echoes who the message reached: the resolved mentions, and
// any @-tokens that matched nobody (so nobody was notified for them). File
// is set when the message carried one: the server's description of it.
type SendResponse struct {
	ID         string    `json:"id"`
	Mentions   []Mention `json:"mentions,omitempty"`
	Unresolved []string  `json:"unresolved,omitempty"`
	File       *File     `json:"file,omitempty"`
}

// A message with a file is sent as
//
//	POST /api/rooms/{room}/files?pid=<pid>&name=<file name>&body=<caption>
//
// with the file's bytes as the request body and their count in
// Content-Length, which is required. Who is sending and how much is in the
// request's head, so the server can refuse before it reads the file, and
// reads exactly what it was told to expect.

type TaskRequest struct {
	PID  string `json:"pid"`
	Task string `json:"task"`
}

type LeaveRequest struct {
	PID string `json:"pid"`
}

type EventsResponse struct {
	Events []Event `json:"events"`
}

type RoomInfo struct {
	Name         string    `json:"name"`
	Created      time.Time `json:"created"`
	LastActivity time.Time `json:"last_activity"`
	Participants int       `json:"participants"`
}

// ParticipantInfo is one entry of the participants listing (presence
// panel). Presence is derived from LastSeen, not tracked.
type ParticipantInfo struct {
	Identity Identity  `json:"identity"`
	Joined   time.Time `json:"joined"`
	LastSeen time.Time `json:"last_seen"`
}

// VersionResponse identifies the server. MaxFile is the largest file, in
// bytes, a message may carry; zero (or absent, from a server that predates
// files) means this server takes none.
type VersionResponse struct {
	Version string `json:"version"`
	Build   string `json:"build,omitempty"`
	Proto   string `json:"proto"`
	MaxFile int64  `json:"max_file,omitempty"`
}

// ErrorBody is the JSON body of every non-2xx API response. Error is a
// stable machine-readable code; Message is for humans.
//
// The two 410 cases are distinguished by code: ErrPIDGone means the pid (or
// its room) no longer exists — rejoin, redeclare your task, continue.
// ErrCursorGone means the events cursor fell off the room's ring buffer —
// the pid is fine; resume from the present.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Stable error codes.
const (
	ErrPIDGone          = "pid_gone"
	ErrCursorGone       = "cursor_gone"
	ErrRateLimited      = "rate_limited"
	ErrTooLarge         = "too_large"
	ErrBadRequest       = "bad_request"
	ErrUnauthorized     = "unauthorized"
	ErrProtoUnsupported = "proto_unsupported"
	ErrNotFound         = "not_found"
	// ErrForbidden: the token is good, but not for this — a hostname or a pid
	// that belongs to the operator (see server.API).
	ErrForbidden = "forbidden"
)
