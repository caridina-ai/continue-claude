package coninject

import "strings"

// State is what the watcher can reliably tell from the bottom of Claude Code's
// console in a single frame. Only the rate-limit menu has a stable, unique
// marker ("1. Stop and wait"); idle vs busy cannot be told from one frame (no
// reliable static text — the spinner wording varies and the prompt hints come
// and go), so the watcher distinguishes those by whether the screen is *moving*
// between two reads, not via Classify.
type State int

const (
	// StateUnknown: not the rate-limit menu — could be idle, busy, or anything.
	StateUnknown State = iota
	// StateModal: the rate-limit menu is showing (its "1. Stop and wait" option).
	StateModal
)

func (s State) String() string {
	if s == StateModal {
		return "modal"
	}
	return "unknown"
}

// Classify reports whether the bottom of the console is the rate-limit menu. It
// matches only the actual "1. Stop and wait" option text — what pressing 1
// selects, and unique to that menu — within the bottom few rows, never the
// scrollback, so a transcript that merely quotes the menu cannot trip a false
// modal. Everything else is StateUnknown; the watcher tells idle from busy by
// whether the screen moves between two reads.
func Classify(screen string) State {
	lines := make([]string, 0, 64)
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return StateUnknown
	}
	n := len(lines)
	tail := strings.Join(lines[max(0, n-6):], "\n")
	if strings.Contains(tail, "1. Stop and wait") {
		return StateModal
	}
	return StateUnknown
}

// IsRateLimited reports whether the screen shows Claude Code's inline rate-limit
// rejection — the "You've hit your … limit · resets …" line printed under a
// prompt that was refused because the window is exhausted. This is the
// idle-prompt form of a block (the session sits at the prompt, no menu), and is
// distinct from the /rate-limit-options menu that Classify detects.
//
// It exists because a freshly-started session whose very first call is blocked
// may never carry structured rate-limit data in its status JSON — so the only
// on-screen evidence of the block is this line, not the menu. Callers pair it
// with a future-reset check (parseScreenReset) to avoid acting on a stale
// rejection still sitting in the scrollback of a session that has recovered.
func IsRateLimited(screen string) bool {
	for _, l := range strings.Split(screen, "\n") {
		low := strings.ToLower(l)
		if strings.Contains(low, "hit your") && strings.Contains(low, "limit") {
			return true
		}
	}
	return false
}
