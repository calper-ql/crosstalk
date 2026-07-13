// Package tui is the human face of crosstalk: a Bubble Tea app with a
// two-section room/occupants sidebar, chat viewport, and an always-focused
// textarea input. The interaction model is click-first — the mouse
// navigates, the keyboard writes.
//
// The TUI keeps its pids in memory only — they are not CLI sessions and
// never touch sessions.toml. Every room opened this session keeps a
// background long-poll running, which is what powers unread and mention
// badges; all rooms are left on quit.
package tui

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

// Run starts the TUI against the default (or named) server alias.
func Run(alias string) error {
	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	alias, entry, err := reg.Lookup(alias)
	if err != nil {
		return err
	}
	prof, err := client.LoadProfile()
	if err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	m := newModel(client.NewConn(entry), alias, prof.Name, hostname)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := p.Run()
	if fm, ok := final.(model); ok {
		// Best-effort goodbye so presence stays honest.
		for _, rv := range fm.joined {
			if rv.pid != "" && !rv.dead {
				_ = fm.conn.Leave(rv.name, rv.pid)
			}
		}
	}
	return err
}

// roomView is the state of a room joined this session.
type roomView struct {
	name         string
	pid          string
	cursor       string
	events       []proto.Event
	participants []proto.ParticipantInfo
	unread       int
	mention      bool
	oldestID     string
	backfillDone bool
	dead         bool // pid_gone or room_closed; reopen rejoins
}

type model struct {
	conn     *client.Conn
	alias    string
	name     string
	hostname string

	needName  bool
	nameInput textinput.Model

	rooms  []proto.RoomInfo
	active string
	joined map[string]*roomView

	vp     viewport.Model
	ta     textarea.Model
	width  int
	height int
	status string
	ready  bool

	// Sidebar layout: draggable width, independent per-section scroll,
	// and a rooms filter.
	sbW         int // sidebar width in columns; dragged via the divider
	dragging    bool
	roomsScroll int
	whoScroll   int
	search      textinput.Model
	searching   bool // search box has focus (keys filter rooms, not chat)

	// @-autocomplete popup state.
	mentionOpen bool
	mentionOpts []mentionOpt
	mentionIdx  int

	// :emoji: autocomplete popup state.
	emojiOpen bool
	emojiOpts []emojiEntry
	emojiIdx  int
}

// mentionOpt is one row of the @-autocomplete popup.
type mentionOpt struct {
	label  string // what the popup shows (identity + task)
	insert string // what completion inserts after the @
}

func newModel(conn *client.Conn, alias, name, hostname string) model {
	ti := textinput.New()
	ti.Placeholder = "your display name"
	ti.CharLimit = 32
	ti.Focus()

	ta := textarea.New()
	ta.Placeholder = "message — Enter sends, Alt+Enter for a newline · click a room to open it"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 16 * 1024
	ta.Focus() // the message input is focused by default; rooms are clicked

	sr := textinput.New()
	sr.Prompt = "/ " // ascii-safe search affordance (renders everywhere)
	sr.Placeholder = "search rooms"
	sr.CharLimit = 32

	return model{
		conn:      conn,
		alias:     alias,
		name:      name,
		hostname:  hostname,
		needName:  name == "",
		nameInput: ti,
		ta:        ta,
		search:    sr,
		sbW:       defaultSidebarW,
		joined:    map[string]*roomView{},
	}
}

// filteredRooms is the room list narrowed by the search box (case-insensitive
// substring on the room name); the full list when the box is empty.
func (m model) filteredRooms() []proto.RoomInfo {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" {
		return m.rooms
	}
	out := m.rooms[:0:0]
	for _, r := range m.rooms {
		if strings.Contains(strings.ToLower(r.Name), q) {
			out = append(out, r)
		}
	}
	return out
}

// clampSidebar keeps the draggable divider within sane bounds: wide enough
// to show room names, leaving room for the chat pane.
func clampSidebar(x, total int) int {
	maxW := total - 30
	if x < minSidebarW {
		x = minSidebarW
	}
	if maxW >= minSidebarW && x > maxW {
		x = maxW
	}
	return x
}

func (m model) Init() tea.Cmd {
	if m.needName {
		return textinput.Blink
	}
	return tea.Batch(fetchRooms(m.conn), roomsTick())
}

// ── messages ────────────────────────────────────────────────────────────

type roomsMsg struct {
	rooms []proto.RoomInfo
	err   error
}
type roomsTickMsg struct{}

type joinedMsg struct {
	room    string
	pid     string
	cursor  string
	history []proto.Event
	parts   []proto.ParticipantInfo
	err     error
}

type eventsMsg struct {
	room   string
	pid    string // poll owner; stale polls are dropped
	events []proto.Event
	err    error
	gone   bool
}

type backfillMsg struct {
	room   string
	events []proto.Event
	err    error
}

type partsMsg struct {
	room  string
	parts []proto.ParticipantInfo
}

type sentMsg struct {
	room string
	err  error
}

// ── commands ────────────────────────────────────────────────────────────

func roomsTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return roomsTickMsg{} })
}

func fetchRooms(c *client.Conn) tea.Cmd {
	return func() tea.Msg {
		rooms, err := c.Rooms()
		return roomsMsg{rooms, err}
	}
}

func joinRoom(c *client.Conn, room, name, hostname string) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.Join(room, proto.JoinRequest{
			Mode: proto.ModeHuman, Hostname: hostname, Name: name,
		})
		if err != nil {
			return joinedMsg{room: room, err: err}
		}
		history, _ := c.History(room, "", 200)
		parts, _ := c.Participants(room)
		return joinedMsg{room: room, pid: resp.PID, cursor: resp.Cursor, history: history, parts: parts}
	}
}

func pollRoom(c *client.Conn, room, pid, cursor string) tea.Cmd {
	return func() tea.Msg {
		evs, err := c.Events(room, pid, cursor, 25*time.Second)
		if err != nil {
			if ae, ok := err.(*client.APIError); ok {
				switch ae.Code {
				case proto.ErrPIDGone:
					return eventsMsg{room: room, pid: pid, err: err, gone: true}
				case proto.ErrCursorGone:
					// Resume from the present: an empty result with no error
					// makes the update loop refetch from a fresh join.
					return eventsMsg{room: room, pid: pid, err: err, gone: true}
				}
			}
			time.Sleep(2 * time.Second) // transient: back off before the retry poll
			return eventsMsg{room: room, pid: pid, err: err}
		}
		return eventsMsg{room: room, pid: pid, events: evs}
	}
}

func backfill(c *client.Conn, room, before string) tea.Cmd {
	return func() tea.Msg {
		evs, err := c.History(room, before, 100)
		return backfillMsg{room, evs, err}
	}
}

func fetchParts(c *client.Conn, room string) tea.Cmd {
	return func() tea.Msg {
		parts, _ := c.Participants(room)
		return partsMsg{room, parts}
	}
}

func sendMessage(c *client.Conn, room, pid, body string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Send(room, pid, body, proto.KindText)
		return sentMsg{room, err}
	}
}

func ringBell() tea.Msg {
	os.Stderr.WriteString("\a")
	return nil
}

// ── update ──────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		if m.active != "" {
			m.refreshViewport(true)
		}
		return m, nil

	case tea.KeyMsg:
		if m.needName {
			return m.updateNamePrompt(msg)
		}
		return m.updateKeys(msg)

	case tea.MouseMsg:
		return m.updateMouse(msg)

	case roomsTickMsg:
		return m, tea.Batch(fetchRooms(m.conn), roomsTick())

	case roomsMsg:
		if msg.err != nil {
			m.status = "room list: " + msg.err.Error()
			return m, nil
		}
		m.rooms = sortRooms(msg.rooms)
		return m, nil

	case joinedMsg:
		if msg.err != nil {
			m.status = "join " + msg.room + ": " + msg.err.Error()
			return m, nil
		}
		rv := &roomView{
			name: msg.room, pid: msg.pid, cursor: msg.cursor,
			events: msg.history, participants: msg.parts,
		}
		if len(msg.history) > 0 {
			rv.oldestID = msg.history[0].ID
		} else {
			rv.backfillDone = true
		}
		m.joined[msg.room] = rv
		m.active = msg.room
		m.status = ""
		m.refreshViewport(true)
		return m, pollRoom(m.conn, rv.name, rv.pid, rv.cursor)

	case eventsMsg:
		rv := m.joined[msg.room]
		if rv == nil || rv.pid != msg.pid {
			return m, nil // stale poll from a room we re-joined or dropped
		}
		if msg.gone {
			rv.dead = true
			m.status = msg.room + ": session ended (" + msg.err.Error() + ") — Enter reopens"
			return m, nil
		}
		if msg.err != nil {
			m.status = msg.room + ": " + msg.err.Error()
			return m, pollRoom(m.conn, rv.name, rv.pid, rv.cursor)
		}
		var cmds []tea.Cmd
		presenceChanged := false
		for _, ev := range msg.events {
			rv.events = append(rv.events, ev)
			rv.cursor = ev.ID
			switch ev.Type {
			case proto.TypeMessage:
				if msg.room != m.active {
					rv.unread++
				}
				if m.mentionsMe(ev) {
					if msg.room != m.active {
						rv.mention = true
					}
					cmds = append(cmds, func() tea.Msg { return ringBell() })
				}
			case proto.TypePresence:
				presenceChanged = true
			case proto.TypeRoomClosed:
				rv.dead = true
				m.status = msg.room + ": room closed (idle expiry)"
			}
		}
		if presenceChanged {
			cmds = append(cmds, fetchParts(m.conn, msg.room))
		}
		if msg.room == m.active {
			m.refreshViewport(m.vp.AtBottom())
		}
		if !rv.dead {
			cmds = append(cmds, pollRoom(m.conn, rv.name, rv.pid, rv.cursor))
		}
		return m, tea.Batch(cmds...)

	case backfillMsg:
		rv := m.joined[msg.room]
		if rv == nil || msg.err != nil {
			return m, nil
		}
		if len(msg.events) == 0 {
			rv.backfillDone = true
			return m, nil
		}
		rv.events = append(append([]proto.Event{}, msg.events...), rv.events...)
		rv.oldestID = msg.events[0].ID
		if msg.room == m.active {
			// Keep the reading position stable-ish after prepending.
			m.refreshViewport(false)
		}
		return m, nil

	case partsMsg:
		if rv := m.joined[msg.room]; rv != nil {
			rv.participants = msg.parts
		}
		return m, nil

	case sentMsg:
		if msg.err != nil {
			m.status = "send: " + msg.err.Error()
		}
		return m, nil
	}

	return m, nil
}

func (m model) updateNamePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		return m, tea.Quit
	case tea.KeyEnter:
		name := m.nameInput.Value()
		if name == "" {
			return m, nil
		}
		m.name = name
		m.needName = false
		_ = client.SaveProfile(&client.Profile{Name: name})
		return m, tea.Batch(fetchRooms(m.conn), roomsTick())
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Search box focused: keystrokes filter the room list.
	if m.searching {
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.search.SetValue("")
			return m.blurSearch()
		case "enter":
			rooms := m.filteredRooms()
			mm, cmd := m.blurSearch()
			m = mm.(model)
			if len(rooms) > 0 {
				m2, cmd2 := m.openRoom(rooms[0].Name)
				return m2, tea.Batch(cmd, cmd2)
			}
			return m, cmd
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.roomsScroll = 0 // a narrowed list starts from the top
		return m, cmd
	}

	// An open autocomplete popup (@mention or :emoji:) captures navigation
	// and completion keys before anything else.
	if m.mentionOpen || m.emojiOpen {
		idx, count := &m.mentionIdx, len(m.mentionOpts)
		if m.emojiOpen {
			idx, count = &m.emojiIdx, len(m.emojiOpts)
		}
		switch msg.String() {
		case "up", "ctrl+p":
			if *idx > 0 {
				*idx--
			}
			return m, nil
		case "down", "ctrl+n":
			if *idx < count-1 {
				*idx++
			}
			return m, nil
		case "tab", "enter":
			if m.emojiOpen {
				m.completeEmoji()
			} else {
				m.completeMention()
			}
			return m, nil
		case "esc":
			m.mentionOpen = false
			m.emojiOpen = false
			return m, nil
		}
	}

	switch {
	// Quit lives on ctrl+q (and conventional ctrl+c), and works no matter
	// what is focused — the exit button carries the visible hint.
	case msg.String() == "ctrl+q" || msg.String() == "ctrl+c":
		return m, tea.Quit
	case msg.Type == tea.KeyEnter && !msg.Alt:
		body := expandShortcodes(m.ta.Value())
		if body == "" {
			return m, nil
		}
		rv := m.joined[m.active]
		if rv == nil || rv.dead {
			m.status = "click a room to open it first"
			return m, nil
		}
		m.ta.Reset()
		return m, sendMessage(m.conn, rv.name, rv.pid, body)
	case msg.Type == tea.KeyEnter && msg.Alt:
		m.ta.InsertString("\n")
		return m, nil
	case msg.String() == "pgup":
		m.vp.PageUp()
		return m.maybeBackfill()
	case msg.String() == "pgdown":
		m.vp.PageDown()
		return m, nil
	}

	// Everything else — including Tab, which is just a tab — goes to the
	// message input.
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.syncPopups()
	return m, cmd
}

func (m model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Dragging the sidebar/chat divider takes precedence over everything.
	if m.dragging {
		if msg.Action == tea.MouseActionRelease {
			m.dragging = false
			return m, nil
		}
		m.sbW = clampSidebar(msg.X, m.width)
		m.layout()
		m.refreshViewport(m.vp.AtBottom())
		return m, nil
	}

	roomsBottom := roomsTopRow + m.roomsSectionHeight() // first row past the room list

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if msg.X > m.sbW {
			m.vp.LineUp(3)
			return m.maybeBackfill()
		}
		if msg.Y < roomsBottom {
			m.roomsScroll = max(0, m.roomsScroll-3)
		} else {
			m.whoScroll = max(0, m.whoScroll-3)
		}
	case tea.MouseButtonWheelDown:
		if msg.X > m.sbW {
			m.vp.LineDown(3)
			return m, nil
		}
		if msg.Y < roomsBottom {
			m.roomsScroll += 3 // clamped against content at render time
		} else {
			m.whoScroll += 3
		}
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		// The divider column (the sidebar's right border) starts a drag.
		if msg.X == m.sbW || msg.X == m.sbW-1 {
			m.dragging = true
			return m, nil
		}
		// Exit button, bottom-right corner of the status row.
		if msg.Y == m.height-1 && msg.X >= m.width-lipglossWidth(exitButtonLabel) {
			return m, tea.Quit
		}
		// Clicking the search box (row 1 of the sidebar) focuses it.
		if msg.X < m.sbW && msg.Y == 1 {
			return m.focusSearch()
		}
		// Clicking anywhere else in the chat/message pane returns focus to
		// the message input.
		if msg.X > m.sbW {
			return m.blurSearch()
		}
		// Rooms are clicked, not keyboard-navigated. The click maps through
		// the filtered list and the section's scroll offset.
		if msg.X < m.sbW && msg.Y >= roomsTopRow && msg.Y < roomsBottom {
			rooms := m.filteredRooms()
			idx := msg.Y - roomsTopRow + m.roomsScroll
			if idx >= 0 && idx < len(rooms) {
				mm, cmd := m.blurSearch()
				m = mm.(model)
				m2, cmd2 := m.openRoom(rooms[idx].Name)
				return m2, tea.Batch(cmd, cmd2)
			}
		}
	}
	return m, nil
}

// focusSearch moves keyboard focus to the rooms filter.
func (m model) focusSearch() (tea.Model, tea.Cmd) {
	if m.searching {
		return m, nil
	}
	m.searching = true
	m.ta.Blur()
	return m, m.search.Focus()
}

// blurSearch returns keyboard focus to the message input and clears the
// filter, so the full room list is always restored on leaving the box.
func (m model) blurSearch() (tea.Model, tea.Cmd) {
	if !m.searching {
		return m, nil
	}
	m.searching = false
	m.search.SetValue("")
	m.search.Blur()
	m.roomsScroll = 0
	return m, m.ta.Focus()
}

// roomsSectionHeight is the line budget of the sidebar's top (rooms)
// section; the occupants section gets the rest below the separator.
func (m model) roomsSectionHeight() int {
	// total sidebar lines minus: rooms header, search box, separator,
	// occupants header
	avail := m.height - statusHeight - 4
	if avail < 2 {
		return 1
	}
	return avail / 2
}

func (m model) whoSectionHeight() int {
	avail := m.height - statusHeight - 4
	h := avail - m.roomsSectionHeight()
	if h < 1 {
		return 1
	}
	return h
}

func (m model) openRoom(room string) (tea.Model, tea.Cmd) {
	if rv, ok := m.joined[room]; ok && !rv.dead {
		m.active = room
		rv.unread = 0
		rv.mention = false
		m.refreshViewport(true)
		return m, nil
	}
	m.status = "joining " + room + "…"
	return m, joinRoom(m.conn, room, m.name, m.hostname)
}

func (m model) maybeBackfill() (tea.Model, tea.Cmd) {
	rv := m.joined[m.active]
	if rv != nil && m.vp.AtTop() && !rv.backfillDone && rv.oldestID != "" {
		return m, backfill(m.conn, rv.name, rv.oldestID)
	}
	return m, nil
}

// mentionPrefixRe matches an @token being typed at the end of the input:
// autocomplete triggers while the cursor is at the end of the text, which
// is where mentions get typed in practice. emojiPrefixRe is the same idea
// for :shortcodes (at least one character, so a lone colon stays quiet).
var mentionPrefixRe = regexp.MustCompile(`(?s)(?:^|[\s(])@([\p{L}\p{N}._/-]*)$`)
var emojiPrefixRe = regexp.MustCompile(`(?s)(?:^|\s):([a-z0-9_+-]+)$`)

// syncPopups recomputes both popups; the mention popup wins if both match.
func (m *model) syncPopups() {
	m.syncMentionPopup()
	if m.mentionOpen {
		m.emojiOpen = false
		return
	}
	m.emojiOpen = false
	match := emojiPrefixRe.FindStringSubmatch(m.ta.Value())
	if match == nil {
		return
	}
	opts := emojiMatches(strings.ToLower(match[1]), 5)
	if len(opts) == 0 {
		return
	}
	m.emojiOpts = opts
	if m.emojiIdx >= len(opts) {
		m.emojiIdx = 0
	}
	m.emojiOpen = true
}

// completeEmoji replaces the :prefix being typed with the selected emoji.
func (m *model) completeEmoji() {
	if m.emojiIdx >= len(m.emojiOpts) {
		return
	}
	value := m.ta.Value()
	i := strings.LastIndex(value, ":")
	if i < 0 {
		return
	}
	m.ta.SetValue(value[:i] + m.emojiOpts[m.emojiIdx].Emoji + " ")
	m.emojiOpen = false
	m.emojiIdx = 0
}

// syncMentionPopup recomputes the popup after every keystroke in the input.
func (m *model) syncMentionPopup() {
	m.mentionOpen = false
	rv := m.joined[m.active]
	if rv == nil {
		return
	}
	match := mentionPrefixRe.FindStringSubmatch(m.ta.Value())
	if match == nil {
		return
	}
	prefix := strings.ToLower(match[1])

	var opts []mentionOpt
	for _, p := range rv.participants {
		full := p.Identity.Hostname + "/" + p.Identity.Name
		if p.Identity.Hostname == m.hostname && p.Identity.Name == m.name {
			continue // no self-tagging in the selector
		}
		if prefix == "" ||
			strings.HasPrefix(strings.ToLower(full), prefix) ||
			strings.HasPrefix(strings.ToLower(p.Identity.Name), prefix) {
			label := full
			if p.Identity.Task != "" {
				label += " — " + p.Identity.Task
			}
			opts = append(opts, mentionOpt{label: label, insert: full})
		}
	}
	if prefix == "" || strings.HasPrefix("room", prefix) {
		opts = append(opts, mentionOpt{label: "room — everyone here", insert: "room"})
	}
	if len(opts) == 0 {
		return
	}
	if len(opts) > 5 {
		opts = opts[:5]
	}
	m.mentionOpts = opts
	if m.mentionIdx >= len(opts) {
		m.mentionIdx = 0
	}
	m.mentionOpen = true
}

// completeMention replaces the @prefix being typed with the selected
// participant and closes the popup.
func (m *model) completeMention() {
	if m.mentionIdx >= len(m.mentionOpts) {
		return
	}
	value := m.ta.Value()
	i := strings.LastIndex(value, "@")
	if i < 0 {
		return
	}
	m.ta.SetValue(value[:i] + "@" + m.mentionOpts[m.mentionIdx].insert + " ")
	m.mentionOpen = false
	m.mentionIdx = 0
}

func (m *model) mentionsMe(ev proto.Event) bool {
	for _, mn := range ev.Mentions {
		if mn.All || (mn.Hostname == m.hostname && mn.Name == m.name) {
			return true
		}
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fmtClock(t time.Time) string { return t.Local().Format("15:04") }

var _ = fmt.Sprintf // keep fmt imported for render.go helpers
