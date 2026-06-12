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
	if err := fs.Parse(args); err != nil {
		return err
	}

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

	// statusLineMode means the status line spawned us and owns armed.lock; only
	// then do we clear it on exit. Ad-hoc runs never touch the lock.
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
	logf := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		// Best-effort terminal feedback (lost once we FreeConsole to attach).
		fmt.Fprintln(os.Stderr, "continue-claude watch:", line)
		if logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s\t%s\n", time.Now().Format("2006-01-02 15:04:05"), line)
	}

	fireAt := resetAt.Add(*delay)
	logf("armed pid=%d reset=%s fireAt=%s", target, resetAt.Format("15:04"), fireAt.Format("15:04:05"))
	if d := time.Until(fireAt); d > 0 {
		time.Sleep(d)
	}

	if statusLineMode {
		defer clearLock(*stateDir, target)
	}

	deadline := fireAt.Add(*timeout)
	for {
		screen, err := coninject.ReadScreen(target)
		state := coninject.StateUnknown
		if err == nil {
			state = coninject.Classify(screen)
		}

		switch state {
		case coninject.StateModal:
			err := coninject.Inject(target, unlockSteps())
			logf("pid=%d modal -> inject 1+continue (err=%v)", target, err)
			return err
		case coninject.StateIdle:
			err := coninject.Inject(target, idleSteps())
			logf("pid=%d idle -> inject continue-prompt (err=%v)", target, err)
			return err
		case coninject.StateBusy:
			logf("pid=%d busy -> stand down (not blocked)", target)
			return nil
		default:
			if time.Now().After(deadline) {
				logf("pid=%d unknown -> timeout, stand down (readErr=%v)", target, err)
				return nil
			}
			time.Sleep(15 * time.Second)
		}
	}
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
