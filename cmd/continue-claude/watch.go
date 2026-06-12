package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	pid := fs.Uint("pid", 0, "target claude.exe PID")
	reset := fs.Int64("reset", 0, "rate-limit reset time (unix seconds)")
	delay := fs.Duration("delay", 3*time.Minute, "delay after reset before checking")
	stateDir := fs.String("state", "", "state directory (for lock cleanup and log)")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up if no actionable state appears")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pid == 0 || *reset == 0 {
		return fmt.Errorf("missing -pid or -reset")
	}

	logPath := ""
	if *stateDir != "" {
		logPath = filepath.Join(*stateDir, "watch.log")
	}
	logf := func(format string, a ...any) {
		if logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s\t%s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
	}

	fireAt := time.Unix(*reset, 0).Add(*delay)
	logf("watch armed pid=%d reset=%s fireAt=%s", *pid, time.Unix(*reset, 0).Format("15:04:05"), fireAt.Format("15:04:05"))
	if d := time.Until(fireAt); d > 0 {
		time.Sleep(d)
	}

	defer clearLock(*stateDir)

	deadline := fireAt.Add(*timeout)
	for {
		screen, err := coninject.ReadScreen(uint32(*pid))
		state := coninject.StateUnknown
		if err == nil {
			state = coninject.Classify(screen)
		}

		switch state {
		case coninject.StateModal:
			err := coninject.Inject(uint32(*pid), unlockSteps())
			logf("modal -> inject 1+continue (err=%v)", err)
			return err
		case coninject.StateIdle:
			err := coninject.Inject(uint32(*pid), idleSteps())
			logf("idle -> inject continue-prompt (err=%v)", err)
			return err
		case coninject.StateBusy:
			logf("busy -> stand down (not blocked)")
			return nil
		default:
			if time.Now().After(deadline) {
				logf("unknown -> timeout, stand down (readErr=%v)", err)
				return nil
			}
			time.Sleep(15 * time.Second)
		}
	}
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

func clearLock(stateDir string) {
	if stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(stateDir, "armed.lock"))
}
