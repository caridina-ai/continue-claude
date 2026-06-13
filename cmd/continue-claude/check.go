package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caridina-ai/continue-claude/internal/coninject"
)

// checkOutcome is the decision for one claude session during a `check` sweep.
type checkOutcome int

const (
	// outcomeSkipNotModal: the session is not showing the rate-limit menu.
	outcomeSkipNotModal checkOutcome = iota
	// outcomeArm: the session is blocked and its reset time was parsed.
	outcomeArm
	// outcomeUnparseable: blocked, but the reset line could not be parsed.
	outcomeUnparseable
)

// classifyForCheck decides what `check` should do with a single screen: arm a
// watcher (returning the reset), skip it (not blocked), or flag it as blocked
// with an unparseable reset (returning the raw text for diagnosis).
func classifyForCheck(screen string, now time.Time) (resetAt time.Time, raw string, outcome checkOutcome) {
	if coninject.Classify(screen) != coninject.StateModal {
		return time.Time{}, "", outcomeSkipNotModal
	}
	resetAt, raw, ok := parseScreenReset(screen, now)
	if !ok {
		return time.Time{}, raw, outcomeUnparseable
	}
	return resetAt, raw, outcomeArm
}

// runCheck is the general external entry point: scan every running claude.exe,
// and for each one stuck at the rate-limit menu, read its reset time off the
// screen and arm a watcher that will unblock it after the reset. Multiple
// sessions may carry different reset times; each watcher sleeps on its own.
//
// It is idempotent via the per-claude armed-<pid>.lock: a session already owned
// by a watcher (from a prior check or from the status line) is left alone.
func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	stateDir := fs.String("state", "", "state directory for locks + log (default ~/.continue-claude)")
	delay := fs.Duration("delay", 3*time.Minute, "delay after each reset before the watcher acts")
	dryRun := fs.Bool("dry-run", false, "report what would be armed without spawning watchers")
	debug := fs.Bool("debug", false, "propagate verbose logging to the watchers this spawns")
	if err := fs.Parse(args); err != nil {
		return err
	}
	debugLog = *debug

	dir := *stateDir
	if dir == "" {
		dir, _ = defaultStateDir()
	}
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		sweepStaleLocks(dir)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	pids, err := coninject.ListClaudePIDs()
	if err != nil {
		return err
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	if len(pids) == 0 {
		fmt.Fprintln(os.Stderr, "check: no claude.exe found")
		return nil
	}
	fmt.Fprintf(os.Stderr, "check: scanning %d claude session(s); decisions also logged to %s\n",
		len(pids), filepath.Join(dir, "watch.log"))

	now := time.Now()
	for _, pid := range pids {
		// Read the screen in a child process: ReadScreen calls FreeConsole, which
		// would tear down our own terminal and break output for the rest of the
		// loop. The child does the console attach and writes the text to a file.
		screen, err := snapshotViaChild(exe, pid)
		if err != nil {
			report(dir, "check pid=%d read error: %v", pid, err)
			continue
		}

		resetAt, raw, outcome := classifyForCheck(screen, now)
		switch outcome {
		case outcomeSkipNotModal:
			fmt.Fprintf(os.Stderr, "check: pid=%d not blocked (%s) -> skip\n", pid, coninject.Classify(screen))
		case outcomeUnparseable:
			report(dir, "check pid=%d blocked but reset unparseable (raw=%q) -> skip", pid, raw)
		case outcomeArm:
			if *dryRun {
				report(dir, "check pid=%d blocked, reset=%s -> would arm (fire=%s)",
					pid, resetAt.Format("15:04"), resetAt.Add(*delay).Format("15:04:05"))
				continue
			}
			armWatcher(dir, exe, pid, resetAt.Unix(), *delay)
		}
	}
	return nil
}

// snapshotViaChild runs `continue-claude snapshot -pid <pid> -out <tmp>` and
// returns the captured screen text, isolating the console-tearing read from the
// parent so the parent's stdout/stderr survive across the whole sweep.
func snapshotViaChild(exe string, pid uint32) (string, error) {
	tmp, err := os.CreateTemp("", fmt.Sprintf("cc-snap-%d-*.txt", pid))
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	cmd := exec.Command(exe, "snapshot", "-pid", strconv.FormatUint(uint64(pid), 10), "-out", tmpPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	runErr := cmd.Run()

	data, readErr := os.ReadFile(tmpPath)
	body := string(data)
	if strings.HasPrefix(body, "ERROR: ") {
		return "", fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(body, "ERROR: ")))
	}
	if readErr != nil {
		if runErr != nil {
			return "", runErr
		}
		return "", readErr
	}
	return body, nil
}

// armWatcher claims the per-claude lock and spawns a detached watcher for the
// given reset, skipping a session that a live watcher already owns. It mirrors
// the status line's ensureWatcher, but arms an arbitrary discovered PID.
func armWatcher(dir, exe string, claudePID uint32, resetUnix int64, delay time.Duration) {
	if dir == "" {
		return
	}
	lockPath := filepath.Join(dir, lockName(claudePID))
	if existing, alive := readLock(lockPath); alive {
		report(dir, "check pid=%d already armed (reset=%d) -> skip", claudePID, existing)
		return
	}
	_ = os.Remove(lockPath) // drop any dead lock so the atomic claim can win

	claim, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		report(dir, "check pid=%d lost claim to a concurrent run -> skip", claudePID)
		return
	}
	defer claim.Close()

	cmd := exec.Command(exe, watchArgs(
		"-pid", strconv.FormatUint(uint64(claudePID), 10),
		"-reset", strconv.FormatInt(resetUnix, 10),
		"-delay", delay.String(),
		"-state", dir,
	)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNoWindow}
	if err := cmd.Start(); err != nil {
		report(dir, "check pid=%d spawn failed: %v", claudePID, err)
		return
	}
	fmt.Fprintf(claim, "%d %d\n", resetUnix, cmd.Process.Pid)
	report(dir, "check pid=%d armed watcher wpid=%d reset=%s",
		claudePID, cmd.Process.Pid, time.Unix(resetUnix, 0).Format("15:04"))
	_ = cmd.Process.Release()
}

// report prints to stderr (best effort) and appends to watch.log so check's
// decisions sit in the same timeline as the watchers it spawns.
func report(dir, format string, a ...any) {
	fmt.Fprintln(os.Stderr, "check: "+fmt.Sprintf(format, a...))
	logEvent(dir, format, a...)
}
