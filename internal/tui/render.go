package tui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/calper-ql/crosstalk/internal/proto"
)

const (
	sidebarWidth       = 28
	sidebarHeaderLines = 1
	inputHeight        = 3
	statusHeight       = 1
)

// exitButtonLabel sits in the bottom-right corner; it is the one place the
// quit binding is advertised, and clicking it quits.
const exitButtonLabel = " ✕ exit (ctrl+q) "

func lipglossWidth(s string) int { return lipgloss.Width(s) }

var (
	cAccent = lipgloss.AdaptiveColor{Light: "62", Dark: "111"}
	cDim    = lipgloss.AdaptiveColor{Light: "245", Dark: "241"}
	cAgent  = lipgloss.AdaptiveColor{Light: "94", Dark: "215"}
	cHuman  = lipgloss.AdaptiveColor{Light: "29", Dark: "78"}
	cAlert  = lipgloss.AdaptiveColor{Light: "160", Dark: "204"}

	sSidebar     = lipgloss.NewStyle().Width(sidebarWidth).BorderStyle(lipgloss.NormalBorder()).BorderRight(true).BorderForeground(cDim)
	sSideHeader  = lipgloss.NewStyle().Bold(true).Foreground(cAccent).Padding(0, 1)
	sRoom        = lipgloss.NewStyle().Padding(0, 1)
	sRoomActive  = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(cAccent)
	sRoomSel     = lipgloss.NewStyle().Padding(0, 1).Reverse(true)
	sBadge       = lipgloss.NewStyle().Foreground(cAlert).Bold(true)
	sDim         = lipgloss.NewStyle().Foreground(cDim)
	sTime        = lipgloss.NewStyle().Foreground(cDim)
	sAgentName   = lipgloss.NewStyle().Foreground(cAgent).Bold(true)
	sHumanName   = lipgloss.NewStyle().Foreground(cHuman).Bold(true)
	sMention     = lipgloss.NewStyle().Foreground(cAlert)
	sCode        = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "236", Dark: "252"}).Background(lipgloss.AdaptiveColor{Light: "254", Dark: "236"}).Padding(0, 1)
	sStatus      = lipgloss.NewStyle().Foreground(cDim).Padding(0, 1)
	sExitButton  = lipgloss.NewStyle().Foreground(cAlert).Bold(true)
	sPresHead    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sIssueRef    = lipgloss.NewStyle().Foreground(cAccent).Underline(true)
	sPromptFrame = lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(1, 3)
)

var issueRefRe = regexp.MustCompile(`\b[\p{L}\p{N}._-]+#\d+\b`)

var mentionTokenRe = regexp.MustCompile(`@[\p{L}\p{N}._-]+(?:/[\p{L}\p{N}._-]+)?`)

// palette assigns every identity a stable color — the "proper chat" look.
// Indexed by a hash of hostname/name so the same participant is the same
// color everywhere: sender names, @tags, and the presence panel.
var palette = []lipgloss.AdaptiveColor{
	{Light: "26", Dark: "111"},  // blue
	{Light: "130", Dark: "215"}, // orange
	{Light: "29", Dark: "78"},   // green
	{Light: "125", Dark: "212"}, // pink (deliberately not cAlert's 204 — that is reserved for mentions of *you*)
	{Light: "92", Dark: "141"},  // purple
	{Light: "30", Dark: "80"},   // cyan
	{Light: "100", Dark: "179"}, // yellow
	{Light: "124", Dark: "210"}, // salmon
}

func idColor(hostname, name string) lipgloss.Style {
	h := uint32(2166136261)
	for _, c := range []byte(hostname + "/" + name) {
		h = (h ^ uint32(c)) * 16777619
	}
	return lipgloss.NewStyle().Foreground(palette[h%uint32(len(palette))])
}

// sMentionMe is the dedicated color for tags that address the local user.
var sMentionMe = lipgloss.NewStyle().Foreground(cAlert).Bold(true).Underline(true)

// sortRooms orders issue rooms first, then ad-hoc, each by last activity.
func sortRooms(rooms []proto.RoomInfo) []proto.RoomInfo {
	out := append([]proto.RoomInfo(nil), rooms...)
	isIssue := func(n string) bool { return strings.ContainsRune(n, '#') }
	sort.SliceStable(out, func(i, j int) bool {
		ii, ij := isIssue(out[i].Name), isIssue(out[j].Name)
		if ii != ij {
			return ii
		}
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	return out
}

// layout recomputes component sizes from the window size.
func (m *model) layout() {
	chatWidth := m.width - sidebarWidth
	if chatWidth < 20 {
		chatWidth = 20
	}
	vpHeight := m.height - inputHeight - statusHeight - 1
	if vpHeight < 3 {
		vpHeight = 3
	}
	m.vp.Width = chatWidth
	m.vp.Height = vpHeight
	m.ta.SetWidth(chatWidth - 2)
}

// refreshViewport re-renders the active room into the viewport.
func (m *model) refreshViewport(gotoBottom bool) {
	rv := m.joined[m.active]
	if rv == nil {
		m.vp.SetContent(sDim.Render("\n  select a room and press Enter"))
		return
	}
	var b strings.Builder
	if !rv.backfillDone {
		b.WriteString(sDim.Render("  ── scroll up for older history ──"))
		b.WriteString("\n")
	}
	for _, ev := range rv.events {
		b.WriteString(m.renderEvent(ev, m.vp.Width))
		b.WriteString("\n")
	}
	m.vp.SetContent(b.String())
	if gotoBottom {
		m.vp.GotoBottom()
	}
}

// renderEvent renders one event, soft-wrapped to the given width.
func (m *model) renderEvent(ev proto.Event, width int) string {
	wrap := lipgloss.NewStyle().Width(width - 2)
	switch ev.Type {
	case proto.TypePresence:
		who := ""
		if ev.Who != nil {
			who = ev.Who.Display()
		}
		var line string
		switch ev.Event {
		case proto.PresenceJoin:
			line = fmt.Sprintf("· %s joined", who)
			if ev.Who != nil && ev.Who.Task != "" {
				line += " — " + ev.Who.Task
			}
		case proto.PresenceLeave:
			line = fmt.Sprintf("· %s left", who)
		case proto.PresenceTask:
			line = fmt.Sprintf("· %s ⇒ %s", who, ev.Who.Task)
		}
		return wrap.Render(sDim.Render(line))

	case proto.TypeRoomClosed:
		return wrap.Render(sDim.Render("── room closed: " + ev.Reason + " ──"))

	case proto.TypeMessage:
		from := ""
		nameStyle := sAgentName
		if ev.From != nil {
			from = ev.From.Display()
			nameStyle = idColor(ev.From.Hostname, ev.From.Name).Bold(true)
		}
		head := sTime.Render(fmtClock(ev.TS)) + " " + nameStyle.Render(from)

		body := ev.Body
		switch ev.Kind {
		case proto.KindCode:
			return head + "\n" + sCode.Width(width-4).Render(body)
		case proto.KindStatus:
			return wrap.Render(head + " " + sDim.Render(body))
		}
		rendered := highlightRefs(m.styleMentionTags(body))
		if m.mentionsMe(ev) {
			rendered = sMention.Render("▌") + rendered
		}
		return wrap.Render(head + " " + rendered)
	}
	return ""
}

// styleMentionTags colors @tags in message bodies: each tagged participant
// in their own identity color, and a dedicated alert color when the tag
// addresses the local user (@me, bare or host-qualified, and @room).
// Trailing . _ - are re-tried without, mirroring the server's resolution.
func (m *model) styleMentionTags(body string) string {
	rv := m.joined[m.active]
	known := map[string][2]string{} // candidate key -> (hostname, name)
	if rv != nil {
		addID := func(id proto.Identity) {
			known[id.Hostname+"/"+id.Name] = [2]string{id.Hostname, id.Name}
			known[id.Name] = [2]string{id.Hostname, id.Name}
		}
		for _, p := range rv.participants {
			addID(p.Identity)
		}
		for _, ev := range rv.events {
			if ev.From != nil {
				addID(*ev.From)
			}
			if ev.Who != nil {
				addID(*ev.Who)
			}
		}
	}

	return mentionTokenRe.ReplaceAllStringFunc(body, func(tok string) string {
		cand := tok[1:] // strip '@'
		var tail string
		for cand != "" {
			if cand == "room" {
				return sMentionMe.Render("@"+cand) + tail
			}
			if id, ok := known[cand]; ok {
				isMe := id[1] == m.name && (id[0] == m.hostname || !strings.Contains(cand, "/"))
				if isMe {
					return sMentionMe.Render("@"+cand) + tail
				}
				return idColor(id[0], id[1]).Bold(true).Render("@"+cand) + tail
			}
			last := cand[len(cand)-1]
			if last != '.' && last != '_' && last != '-' {
				break
			}
			tail = string(last) + tail
			cand = cand[:len(cand)-1]
		}
		return tok
	})
}

// highlightRefs styles issue references (repo#number) inside message text
// so they stand out as room names.
func highlightRefs(body string) string {
	return issueRefRe.ReplaceAllStringFunc(body, func(ref string) string {
		return sIssueRef.Render(ref)
	})
}

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}
	if m.needName {
		prompt := sPromptFrame.Render(
			sPresHead.Render("welcome to crosstalk") + "\n\n" +
				"pick a display name for this machine:\n\n" +
				m.nameInput.View())
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, prompt)
	}

	main := lipgloss.JoinHorizontal(lipgloss.Top, m.viewSidebar(), m.viewChat())
	mainStyle := lipgloss.NewStyle().Height(m.height - statusHeight).MaxHeight(m.height - statusHeight)
	return mainStyle.Render(main) + "\n" + m.viewStatus()
}

// viewSidebar renders the left column as two independently scrollable
// sections — rooms above, active-room occupants below — split by a
// separator at the middle.
func (m *model) viewSidebar() string {
	// Top section: room rows.
	var roomRows []string
	if len(m.rooms) == 0 {
		roomRows = append(roomRows, sDim.Render(" no rooms yet"))
	}
	for _, r := range m.rooms {
		name := r.Name
		// Right-aligned cluster: an optional unread/mention badge, then the
		// participant count flush to the right edge.
		meta := fmt.Sprintf("%d", r.Participants)
		if rv, ok := m.joined[r.Name]; ok {
			if rv.mention {
				meta = sBadge.Render("@") + " " + meta
			} else if rv.unread > 0 {
				meta = sBadge.Render(fmt.Sprintf("•%d", rv.unread)) + " " + meta
			}
		}
		// Name flush left, meta flush right, filled with spaces between; the
		// name is truncated first if the two would collide.
		maxw := sidebarWidth - 3
		metaW := lipgloss.Width(meta)
		if lipgloss.Width(name)+metaW+1 > maxw {
			name = name[:max(1, maxw-metaW-2)] + "…"
		}
		gap := maxw - lipgloss.Width(name) - metaW
		if gap < 1 {
			gap = 1
		}
		row := name + strings.Repeat(" ", gap) + meta
		style := sRoom
		if r.Name == m.active {
			style = sRoomActive
		}
		roomRows = append(roomRows, style.Render(row))
	}

	// Bottom section: occupant rows (name line + optional dim task line).
	var whoRows []string
	whoTitle := "in —"
	if rv := m.joined[m.active]; rv != nil {
		roomLabel := m.active
		if lipgloss.Width(roomLabel) > sidebarWidth-8 {
			roomLabel = roomLabel[:sidebarWidth-9] + "…"
		}
		whoTitle = "in " + roomLabel
		now := time.Now()
		for _, p := range rv.participants {
			marker := "●"
			if now.Sub(p.LastSeen) > 5*time.Minute {
				marker = sDim.Render("○")
			}
			name := p.Identity.Display()
			if lipgloss.Width(name) > sidebarWidth-5 {
				name = name[:sidebarWidth-6] + "…"
			}
			whoRows = append(whoRows, " "+marker+" "+idColor(p.Identity.Hostname, p.Identity.Name).Render(name))
			if t := p.Identity.Task; t != "" {
				if lipgloss.Width(t) > sidebarWidth-6 {
					t = t[:sidebarWidth-7] + "…"
				}
				whoRows = append(whoRows, sDim.Render("   "+t))
			}
		}
	}

	topH, botH := m.roomsSectionHeight(), m.whoSectionHeight()
	m.roomsScroll = clampScroll(m.roomsScroll, len(roomRows), topH)
	m.whoScroll = clampScroll(m.whoScroll, len(whoRows), botH)

	var b strings.Builder
	b.WriteString(sSideHeader.Render(m.alias + " · rooms" + scrollMark(m.roomsScroll, len(roomRows), topH)))
	b.WriteString("\n")
	b.WriteString(strings.Join(window(roomRows, m.roomsScroll, topH), "\n"))
	b.WriteString("\n")
	b.WriteString(sDim.Render(strings.Repeat("─", sidebarWidth-1)))
	b.WriteString("\n")
	b.WriteString(sSideHeader.Render(whoTitle + scrollMark(m.whoScroll, len(whoRows), botH)))
	b.WriteString("\n")
	b.WriteString(strings.Join(window(whoRows, m.whoScroll, botH), "\n"))
	return m.fitColumn(sSidebar, b.String())
}

// window returns rows[off:off+h] padded with blanks to exactly h lines.
func window(rows []string, off, h int) []string {
	out := make([]string, 0, h)
	for i := off; i < off+h; i++ {
		if i >= 0 && i < len(rows) {
			out = append(out, rows[i])
		} else {
			out = append(out, "")
		}
	}
	return out
}

func clampScroll(off, content, h int) int {
	maxOff := content - h
	if maxOff < 0 {
		maxOff = 0
	}
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// scrollMark appends a subtle indicator to a section header when content
// extends beyond the visible window.
func scrollMark(off, content, h int) string {
	switch {
	case off > 0 && content-off > h:
		return " ↕"
	case off > 0:
		return " ↑"
	case content > h:
		return " ↓"
	}
	return ""
}

// fitColumn renders a column at exactly the main-area height, regardless of
// how content wrapped.
func (m model) fitColumn(style lipgloss.Style, body string) string {
	target := m.height - statusHeight
	return style.Height(target).MaxHeight(target).Render(body)
}

func (m model) viewChat() string {
	title := m.active
	if title == "" {
		title = "crosstalk"
	}
	if rv := m.joined[m.active]; rv != nil && rv.dead {
		title += sDim.Render("  (closed — Enter on the room to rejoin)")
	}
	head := sSideHeader.Render(title)

	if m.mentionOpen || m.emojiOpen {
		// The popup borrows rows from the viewport; m is a copy here, so
		// resizing the viewport for this frame is side-effect free.
		type row struct {
			label    string
			selected bool
		}
		var items []row
		if m.mentionOpen {
			for i, opt := range m.mentionOpts {
				items = append(items, row{"@ " + opt.label, i == m.mentionIdx})
			}
		} else {
			for i, e := range m.emojiOpts {
				items = append(items, row{e.Emoji + "  :" + e.Code + ":", i == m.emojiIdx})
			}
		}
		var rows []string
		for _, it := range items {
			label := it.label
			if lipgloss.Width(label) > m.vp.Width-6 {
				label = label[:m.vp.Width-7] + "…"
			}
			if it.selected {
				rows = append(rows, sRoomSel.Render(label))
			} else {
				rows = append(rows, sRoom.Render(label))
			}
		}
		popup := lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(cAccent).
			Render(strings.Join(rows, "\n"))
		popupH := lipgloss.Height(popup)
		wasBottom := m.vp.AtBottom()
		m.vp.Height -= popupH
		if m.vp.Height < 1 {
			m.vp.Height = 1
		}
		if wasBottom {
			m.vp.GotoBottom()
		}
		return lipgloss.JoinVertical(lipgloss.Left, head, m.vp.View(), popup, m.ta.View())
	}
	return lipgloss.JoinVertical(lipgloss.Left, head, m.vp.View(), m.ta.View())
}

// viewStatus is the bottom row: status text on the left, the exit button —
// the sole home of the quit binding — in the corner.
func (m model) viewStatus() string {
	button := sExitButton.Render(exitButtonLabel)
	leftWidth := m.width - lipgloss.Width(button)
	if leftWidth < 0 {
		leftWidth = 0
	}
	left := sStatus.Width(leftWidth).MaxHeight(1).Render(m.status)
	return lipgloss.JoinHorizontal(lipgloss.Bottom, left, button)
}
