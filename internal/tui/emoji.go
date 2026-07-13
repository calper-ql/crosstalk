package tui

import (
	"regexp"
	"strings"
)

// emojiEntry is one :shortcode: → emoji mapping. The slice is ordered so
// the popup presents stable, roughly-most-useful-first results.
type emojiEntry struct {
	Code  string
	Emoji string
}

var emojiTable = []emojiEntry{
	{"thumbsup", "👍"}, {"thumbsdown", "👎"}, {"ok_hand", "👌"}, {"wave", "👋"},
	{"clap", "👏"}, {"pray", "🙏"}, {"muscle", "💪"}, {"handshake", "🤝"},
	{"salute", "🫡"}, {"shrug", "🤷"}, {"facepalm", "🤦"}, {"point_up", "☝️"},
	{"smile", "😄"}, {"grin", "😁"}, {"joy", "😂"}, {"rofl", "🤣"},
	{"slight_smile", "🙂"}, {"wink", "😉"}, {"sweat_smile", "😅"}, {"cool", "😎"},
	{"thinking", "🤔"}, {"cry", "😢"}, {"sob", "😭"}, {"angry", "😠"},
	{"party", "🥳"}, {"skull", "💀"}, {"ghost", "👻"}, {"robot", "🤖"},
	{"brain", "🧠"}, {"eyes", "👀"}, {"heart", "❤️"}, {"broken_heart", "💔"},
	{"fire", "🔥"}, {"rocket", "🚀"}, {"tada", "🎉"}, {"sparkles", "✨"},
	{"star", "⭐"}, {"zap", "⚡"}, {"boom", "💥"}, {"100", "💯"},
	{"check", "✅"}, {"x", "❌"}, {"warning", "⚠️"}, {"question", "❓"},
	{"exclamation", "❗"}, {"bulb", "💡"}, {"bug", "🐛"}, {"wrench", "🔧"},
	{"hammer", "🔨"}, {"gear", "⚙️"}, {"lock", "🔒"}, {"key", "🔑"},
	{"mag", "🔍"}, {"chart_up", "📈"}, {"chart_down", "📉"}, {"memo", "📝"},
	{"clipboard", "📋"}, {"package", "📦"}, {"computer", "💻"}, {"floppy", "💾"},
	{"hourglass", "⏳"}, {"clock", "🕐"}, {"calendar", "📅"}, {"coffee", "☕"},
	{"beer", "🍺"}, {"pizza", "🍕"}, {"snail", "🐌"}, {"turtle", "🐢"},
	{"dog", "🐶"}, {"cat", "🐱"},
}

var shortcodeRe = regexp.MustCompile(`:([a-z0-9_+-]+):`)

// expandShortcodes replaces every known :code: in an outgoing message with
// its emoji. Unknown codes pass through untouched.
func expandShortcodes(body string) string {
	return shortcodeRe.ReplaceAllStringFunc(body, func(tok string) string {
		code := strings.Trim(tok, ":")
		for _, e := range emojiTable {
			if e.Code == code {
				return e.Emoji
			}
		}
		return tok
	})
}

// emojiMatches returns table entries whose code starts with (then contains)
// the prefix, capped for the popup.
func emojiMatches(prefix string, limit int) []emojiEntry {
	var starts, contains []emojiEntry
	for _, e := range emojiTable {
		if strings.HasPrefix(e.Code, prefix) {
			starts = append(starts, e)
		} else if prefix != "" && strings.Contains(e.Code, prefix) {
			contains = append(contains, e)
		}
	}
	out := append(starts, contains...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
