package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caridina-ai/continue-claude/internal/coninject"
)

// maxResetAhead bounds how far in the future a genuine pending reset can be: the
// session limit window is 5 hours, so a date-less reset time that resolves to
// more than this ahead is really the *past* occurrence (already reset — act now).
const maxResetAhead = 5 * time.Hour

// settleDelay is how long the watcher waits between its two screen reads when
// telling idle from busy: long enough for a running spinner's per-second counter
// to tick, so a working session reads as "moving" and an idle one as "still".
const settleDelay = 2 * time.Second

// idlePrompt is injected when the session is idle at check time. It states the
// rate-limit recovery as a fact (rather than asking whether it happened, since
// the session may have gone through /rate-limit-options) and is self-correcting:
// it resumes interrupted work, but is harmless if the turn had genuinely
// finished.
const idlePrompt = `We just recovered from a rate limit. If you weren't finished, continue from where you left off; if you were already done, reply "done".`

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	pid := fs.Uint("pid", 0, "target claude.exe PID (0 = auto-detect the blocked claude)")
	reset := fs.Int64("reset", 0, "reset time as unix seconds (the status line uses this)")
	delay := fs.Duration("delay", 3*time.Minute, "delay after reset before checking")
	stateDir := fs.String("state", "", "state directory for the log (default ~/.continue-claude)")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up if no actionable state appears")
	debug := fs.Bool("debug", false, "verbose logging: every poll and unknown-screen dump, not just actions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	debugLog = *debug

	now := time.Now()

	// Reset time: a positional HH:MM argument (ad-hoc use) takes precedence,
	// then -reset unix seconds (status line), else now (check immediately).
	var resetAt time.Time
	switch {
	case fs.NArg() >= 1:
		t, err := parseClockTime(fs.Arg(0), now)
		if err != nil {
			return err
		}
		resetAt = t
	case *reset > 0:
		resetAt = time.Unix(*reset, 0)
	default:
		resetAt = now
	}

	// Target PID: explicit -pid, else auto-detect the blocked claude.
	target := uint32(*pid)
	if target == 0 {
		detected, err := coninject.FindBlockedClaude()
		if err != nil {
			return err
		}
		target = detected
	}

	// statusLineMode means the status line spawned us and owns armed-<pid>.lock;
	// only then do we clear it on exit. Ad-hoc runs never touch the lock.
	statusLineMode := *reset > 0 && *stateDir != ""

	dir := *stateDir
	if dir == "" {
		dir, _ = defaultStateDir()
	}
	logPath := ""
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		logPath = filepath.Join(dir, "watch.log")
	}
	// emit always echoes to stderr (ephemeral interactive feedback, discarded once
	// a detached watcher calls FreeConsole); toFile controls whether the line also
	// lands in watch.log.
	emit := func(toFile bool, format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, "continue-claude watch:", line)
		if !toFile || logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s\t%s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	}
	// logf records actions + lifecycle outcomes (always); logfDebug records the
	// per-poll diagnostics that only reach watch.log in -debug mode.
	logf := func(format string, a ...any) { emit(true, format, a...) }
	logfDebug := func(format string, a ...any) { emit(debugLog, format, a...) }

	// Register the lock cleanup up front so it runs on every exit path.
	if statusLineMode {
		defer clearLock(*stateDir, target)
	}

	fireAt := resetAt.Add(*delay)
	logf("armed pid=%d reset=%s fireAt=%s", target, resetAt.Format("15:04"), fireAt.Format("15:04:05"))

	// Wait until fireAt, but poll the target's liveness so a watcher whose claude
	// has /exited leaves promptly instead of lingering in memory for hours.
	for time.Now().Before(fireAt) {
		if !coninject.IsAlive(target) {
			logf("pid=%d target exited before fireAt -> stand down", target)
			return nil
		}
		d := time.Until(fireAt)
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		time.Sleep(d)
	}

	deadline := fireAt.Add(*timeout)
	for {
		if !coninject.IsAlive(target) {
			logf("pid=%d target exited -> stand down", target)
			return nil
		}

		// First read: if the rate-limit menu is up, select "Stop and wait".
		screen1, err1 := coninject.ReadScreen(target)
		if err1 == nil && coninject.Classify(screen1) == coninject.StateModal {
			err := coninject.Inject(target, unlockSteps())
			logf("pid=%d modal -> inject 1+continue (err=%v)", target, err)
			return err
		}

		// Not the menu. idle vs busy can't be told from a single frame (no
		// reliable static marker), so read again after a beat and see if the
		// screen moved: a running session's spinner ticks every second, an idle
		// prompt stays put (our own status line is ignored — it repaints on its
		// own). A menu that appears in between is still handled.
		time.Sleep(settleDelay)
		if !coninject.IsAlive(target) {
			logf("pid=%d target exited between reads -> stand down", target)
			return nil
		}
		screen2, err2 := coninject.ReadScreen(target)
		if err2 == nil && coninject.Classify(screen2) == coninject.StateModal {
			err := coninject.Inject(target, unlockSteps())
			logf("pid=%d modal -> inject 1+continue (err=%v)", target, err)
			return err
		}

		if err1 == nil && err2 == nil {
			if movedBetween(screen1, screen2) {
				logf("pid=%d busy (screen still moving) -> stand down", target)
				return nil
			}
			err := coninject.Inject(target, idleSteps())
			logf("pid=%d idle (screen settled) -> inject continue-prompt (err=%v)", target, err)
			return err
		}

		// At least one read errored. If the target is gone, stand down now;
		// otherwise treat it as a transient unknown and retry until the deadline.
		if !coninject.IsAlive(target) {
			logf("pid=%d target exited -> stand down", target)
			return nil
		}
		logfDebug("pid=%d unknown screen tail:\n%s", target, screenTail(screen2, err2))
		if time.Now().After(deadline) {
			logf("pid=%d unknown -> timeout, stand down", target)
			return nil
		}
		// settleDelay already slept above; loop and try again.
	}
}

// parseScreenReset extracts the reset time from a blocked screen's
// "... resets 12:40am (Asia/Taipei)" line. It understands the time-only 12-hour
// form (note 12:40am == 00:40); for an unrecognised (e.g. cross-day) form it
// returns the raw text and false so the format can be captured from the log.
//
// The displayed time carries no date, so it is disambiguated against the 5-hour
// window: roll to the next occurrence, but if that lands more than maxResetAhead
// in the future, the reset is the previous (already-passed) occurrence instead —
// otherwise a reset that just elapsed would be read as ~a day away.
func parseScreenReset(screen string, now time.Time) (resetAt time.Time, raw string, ok bool) {
	idx := strings.Index(screen, "resets ")
	if idx < 0 {
		return time.Time{}, "", false
	}
	rest := screen[idx+len("resets "):]
	if cut := strings.IndexAny(rest, "(\n"); cut >= 0 {
		rest = rest[:cut]
	}
	raw = strings.TrimSpace(rest)

	t, err := time.ParseInLocation("3:04pm", strings.ToLower(strings.ReplaceAll(raw, " ", "")), now.Location())
	if err != nil {
		return time.Time{}, raw, false
	}
	resetAt = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if resetAt.Before(now) {
		resetAt = resetAt.Add(24 * time.Hour) // next occurrence at or after now
	}
	// The 5h flip is only sound for the session limit, whose window is 5 hours. A
	// weekly limit can legitimately reset more than 5h ahead, so flip a far-ahead
	// time to the past only when the screen shows this is the session limit; a
	// weekly time-only form (necessarily within 24h, else it carries a date) is
	// left as the next occurrence.
	if isSessionLimit(screen) && resetAt.Sub(now) > maxResetAhead {
		resetAt = resetAt.Add(-24 * time.Hour) // too far ahead -> it already passed
	}
	return resetAt, raw, true
}

// isSessionLimit reports whether the screen is the 5-hour session limit (as
// opposed to the longer weekly limit), which gates the 5h ahead/behind flip.
func isSessionLimit(screen string) bool {
	return strings.Contains(strings.ToLower(screen), "session limit")
}

// screenTail returns the last few non-empty lines of a console snapshot (or the
// read error) for diagnostic logging.
func screenTail(screen string, readErr error) string {
	if readErr != nil {
		return "  <read error: " + readErr.Error() + ">"
	}
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, "  | "+l)
		}
	}
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	if len(lines) == 0 {
		return "  <empty screen>"
	}
	return strings.Join(lines, "\n")
}

// movedBetween reports whether two console reads differ in anything other than
// our own status line (which Claude Code may repaint on its own between reads).
// A running session redraws its spinner / token counter every second; an idle
// prompt is otherwise identical frame to frame.
func movedBetween(a, b string) bool {
	return dropStatusLine(a) != dropStatusLine(b)
}

// dropStatusLine removes our own status line — the only line carrying
// "| context " / "| usage " — so its independent repaints don't count as the
// screen "moving".
func dropStatusLine(screen string) string {
	var keep []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.Contains(l, "| context ") || strings.Contains(l, "| usage ") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// parseClockTime turns an "HH:MM" (or "HH:MM:SS"), 24-hour local clock string
// into the next time that clock reads — today if still ahead, otherwise
// tomorrow. Rate-limit resets are always within a few hours, so "next
// occurrence" is unambiguous in practice.
func parseClockTime(s string, now time.Time) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return time.Time{}, fmt.Errorf("cannot parse time %q (want HH:MM, 24-hour)", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := 0, error(nil)
	if len(parts) == 3 {
		sec, err3 = strconv.Atoi(parts[2])
	}
	if err1 != nil || err2 != nil || err3 != nil ||
		h < 0 || h > 23 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		return time.Time{}, fmt.Errorf("invalid time %q (want HH:MM, 24-hour)", s)
	}
	res := time.Date(now.Year(), now.Month(), now.Day(), h, m, sec, 0, now.Location())
	if res.Before(now) {
		res = res.Add(24 * time.Hour)
	}
	return res, nil
}

func unlockSteps() []coninject.Step {
	return []coninject.Step{
		coninject.Text("1"),
		coninject.Delay(400 * time.Millisecond),
		coninject.Text("continue"),
		coninject.Delay(150 * time.Millisecond),
		coninject.Enter(),
	}
}

func idleSteps() []coninject.Step {
	return []coninject.Step{
		coninject.Text(idlePrompt),
		coninject.Delay(150 * time.Millisecond),
		coninject.Enter(),
	}
}

func clearLock(stateDir string, claudePID uint32) {
	if stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(stateDir, lockName(claudePID)))
}
