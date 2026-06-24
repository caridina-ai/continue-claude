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
// telling idle from busy. It must comfortably exceed the spinner's per-second
// tick so a working session always reads as "moving": at 2s two reads can still
// straddle a single counter value (no visible change), so we use 3s to guarantee
// the counter has advanced. A genuinely idle screen stays "still" regardless.
const settleDelay = 3 * time.Second

// awaitBlockTimeout bounds how long an -await-block watcher waits for the block
// to render before giving up. A genuine rejection appears within seconds of the
// submission that spawned us, so a longer silence means the call was not actually
// blocked (it succeeded, or is a normal long-running turn) and there is nothing
// to arm. awaitBlockPoll is the gap between screen reads while waiting.
const (
	awaitBlockTimeout = 90 * time.Second
	awaitBlockPoll    = 3 * time.Second
)

// maxIdleAttempts caps how many times the idle path re-injects the recovery
// prompt while waiting for it to take. This is only a backstop for keystrokes
// that never reach the console (the inject is a complete no-op, leaving the
// screen unchanged): the moment an inject *does* land — the prompt submits, then
// processes, finishes, errors, or bounces — the watcher stands down rather than
// re-injecting, so a session that simply can't proceed (e.g. a 509 outage) is
// never flooded. So a small cap is enough; we are not waiting out a slow reset
// here (fireAt is already minutes past it).
const maxIdleAttempts = 3

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
	awaitBlock := fs.Bool("await-block", false, "poll the screen for the block to appear and read the reset off it before arming (no -reset needed)")
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
	// only then do we clear it on exit. Ad-hoc runs never touch the lock. An
	// await-block watcher is spawned the same way (it just discovers its reset by
	// polling rather than being handed one), so it owns the lock too.
	statusLineMode := (*reset > 0 || *awaitBlock) && *stateDir != ""

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

	// await-block: we were spawned at the instant a session submitted a prompt
	// while over the limit, before Claude Code rendered the rejection (or any
	// reset into its status JSON) — so there is no reset to wait on yet. Poll the
	// screen until the block appears and its reset can be read off it, then fall
	// through into the normal wait-until-reset flow with that reset.
	if *awaitBlock {
		discovered, ok := awaitBlockReset(target, logf, logfDebug)
		if !ok {
			return nil // target gone or no block appeared; lock cleared by defer
		}
		resetAt = discovered
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
	idleAttempts := 0
	injectedBaseline := "" // screen as it was at our last idle inject ("" = none yet)
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
			// Idle and settled. If we already injected the recovery prompt and the
			// screen has since changed, the prompt landed and the session settled
			// into a new state — finished, errored, or bounced. Re-injecting would
			// only pile more prompts onto a session that can't currently proceed
			// (e.g. a 509 outage, where every submission just errors), so stand
			// down: the job (resubmit once) is done, and spamming helps no one.
			if injectedBaseline != "" && movedBetween(injectedBaseline, screen2) {
				logf("pid=%d idle: recovery prompt landed (screen changed since inject) -> stand down", target)
				return nil
			}
			// First time, or the screen is byte-for-byte what it was when we last
			// injected (our own status line aside) — i.e. the keystrokes never
			// reached the console. (Re-)inject; an inject that does take is caught
			// by the branch above (or by "busy" once it starts processing).
			idleAttempts++
			if err := coninject.Inject(target, idleSteps()); err != nil {
				logf("pid=%d idle -> inject continue-prompt failed (attempt %d, err=%v)", target, idleAttempts, err)
				return err
			}
			logf("pid=%d idle (screen settled) -> inject continue-prompt (attempt %d)", target, idleAttempts)
			injectedBaseline = screen2
			if idleAttempts >= maxIdleAttempts || time.Now().After(deadline) {
				logf("pid=%d idle inject did not land after %d attempt(s) -> stand down", target, idleAttempts)
				return nil
			}
			// Let the inject reach its resting state before re-reading, so the next
			// pass compares against a settled screen rather than a transient frame.
			time.Sleep(settleDelay)
			continue
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

// awaitBlockReset polls the target's screen until a rate-limit block appears and
// its reset can be read off it, returning that reset. It is the leading phase of
// a watcher spawned with -await-block, used when the status line caught a session
// mid-submission while over the limit (no reset in the JSON, rejection not yet on
// screen). The same classification `check` uses decides when a block is present;
// once found we hand the reset back to the normal wait-until-reset flow.
//
// Whether to keep waiting is decided the same way the watcher tells busy from
// idle — by movement between two reads, never by any single-frame "is it
// running" marker (Claude Code has none reliable). A moving screen means a turn
// is in flight and a block may still render, so we wait; a screen that settles
// with no block means nothing is on its way and we stand down. Returns ok=false
// if the target exits, the screen settles unblocked, or awaitBlockTimeout passes.
func awaitBlockReset(target uint32, logf, logfDebug func(string, ...any)) (time.Time, bool) {
	deadline := time.Now().Add(awaitBlockTimeout)
	logfDebug("pid=%d await-block: waiting for the rate-limit screen to render", target)
	for {
		if !coninject.IsAlive(target) {
			logfDebug("pid=%d await-block: target exited -> stand down", target)
			return time.Time{}, false
		}
		screen1, err1 := coninject.ReadScreen(target)
		if r, ok := blockReset(screen1, err1, target, logfDebug); ok {
			logf("pid=%d await-block: block detected, reset=%s", target, r.Format("15:04"))
			return r, true
		}

		time.Sleep(awaitBlockPoll)
		if !coninject.IsAlive(target) {
			logfDebug("pid=%d await-block: target exited -> stand down", target)
			return time.Time{}, false
		}
		screen2, err2 := coninject.ReadScreen(target)
		if r, ok := blockReset(screen2, err2, target, logfDebug); ok {
			logf("pid=%d await-block: block detected, reset=%s", target, r.Format("15:04"))
			return r, true
		}

		// No block on either read. A still screen means no turn is in flight and
		// nothing is coming — stand down. A moving one means a turn is running, so
		// keep waiting (a block may yet render) until the deadline.
		if err1 == nil && err2 == nil && !movedBetween(screen1, screen2) {
			logfDebug("pid=%d await-block: screen settled with no block -> stand down", target)
			return time.Time{}, false
		}
		if time.Now().After(deadline) {
			logfDebug("pid=%d await-block: no block within %s -> stand down", target, awaitBlockTimeout)
			return time.Time{}, false
		}
	}
}

// blockReset reports the reset time if the given screen read shows a rate-limit
// block (menu or inline rejection) with a parseable reset, using the same
// classification as `check`. An unparseable block is logged and treated as
// "not yet" so the caller keeps polling.
func blockReset(screen string, readErr error, target uint32, logfDebug func(string, ...any)) (time.Time, bool) {
	if readErr != nil {
		return time.Time{}, false
	}
	resetAt, raw, outcome := classifyForCheck(screen, time.Now())
	switch outcome {
	case outcomeArm:
		return resetAt, true
	case outcomeUnparseable:
		logfDebug("pid=%d await-block: blocked but reset unparseable (raw=%q)", target, raw)
	}
	return time.Time{}, false
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

	compact := strings.ToLower(strings.ReplaceAll(raw, " ", ""))
	t, err := time.ParseInLocation("3:04pm", compact, now.Location())
	if err != nil {
		// Claude Code drops the ":00" for an on-the-hour reset ("1am", "3pm"),
		// which the minute-bearing layout rejects; accept that form too. Getting
		// this wrong leaves a real block unparseable and unarmed.
		t, err = time.ParseInLocation("3pm", compact, now.Location())
	}
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
