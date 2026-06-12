package coninject

import "strings"

// State is the classification of Claude Code's current on-screen state, derived
// from the bottom region of its console (the live UI, not the scrollback).
type State int

const (
	// StateUnknown means the screen could not be classified — stand down.
	StateUnknown State = iota
	// StateModal means the rate-limit options menu is showing.
	StateModal
	// StateBusy means Claude Code is actively working (interruptible).
	StateBusy
	// StateIdle means an empty prompt is waiting for input.
	StateIdle
)

func (s State) String() string {
	switch s {
	case StateModal:
		return "modal"
	case StateBusy:
		return "busy"
	case StateIdle:
		return "idle"
	default:
		return "unknown"
	}
}

// Classify inspects the bottom region of a console snapshot and decides the
// state. It deliberately matches only the last few live lines, because the
// scrollback transcript can quote phrases like "esc to interrupt" that would
// otherwise cause false positives.
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
	tail := strings.Join(lines[max(0, n-14):], "\n")
	last := strings.Join(lines[max(0, n-3):], "\n")

	switch {
	case strings.Contains(tail, "Stop and wait for limit to reset") &&
		strings.Contains(tail, "Esc to cancel"):
		return StateModal
	case strings.Contains(last, "esc to interrupt"):
		return StateBusy
	case strings.Contains(last, "shift+tab to cycle"):
		return StateIdle
	default:
		return StateUnknown
	}
}
