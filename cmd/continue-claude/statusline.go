package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caridina-ai/continue-claude/internal/coninject"
)

// armWord is the single-word status-line marker shown when a watcher is armed.
// "check" is deliberately non-committal: at the shown time the watcher only
// *checks* the screen state, then may unblock, nudge, or stand down — it does
// not necessarily resume.
const armWord = "check"

const (
	detachedProcess = 0x00000008
	createNoWindow  = 0x08000000
)

type statusInput struct {
	Model struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Effort *struct {
		Level string `json:"level"`
	} `json:"effort"`
	Thinking *struct {
		Enabled bool `json:"enabled"`
	} `json:"thinking"`
	ContextWindow *struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits *rateLimits `json:"rate_limits"`
}

type rateLimits struct {
	FiveHour *rateLimit `json:"five_hour"`
	SevenDay *rateLimit `json:"seven_day"`
}

type rateLimit struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

type statusOptions struct {
	stateDir       string
	usageThreshold float64
	weekThreshold  float64
	postResetDelay time.Duration
}

func runStatusline(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	fs := flag.NewFlagSet("continue-claude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := statusOptions{usageThreshold: 90, weekThreshold: 95, postResetDelay: 3 * time.Minute}
	defaultState, _ := defaultStateDir()
	fs.StringVar(&opts.stateDir, "state", defaultState, "directory for armed-watcher state")
	fs.Float64Var(&opts.usageThreshold, "usage-threshold", opts.usageThreshold, "5-hour usage %% that arms a watcher")
	fs.Float64Var(&opts.weekThreshold, "week-threshold", opts.weekThreshold, "7-day usage %% that arms a watcher")
	fs.DurationVar(&opts.postResetDelay, "post-reset-delay", opts.postResetDelay, "delay after reset before the watcher checks")
	debug := fs.Bool("debug", false, "verbose logging: every tick, poll, and skip — not just actions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The status line is the default mode and takes no positional arguments, so
	// reject any. This makes `continue-claude statusline` a clear error rather
	// than silently running: there is deliberately no "statusline" subcommand.
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown argument %q", fs.Arg(0))
	}
	debugLog = *debug

	var input statusInput
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		return fmt.Errorf("read Claude Code status JSON: %w", err)
	}

	current := time.Now()
	logTick(opts, input)
	line, armed := formatStatusLine(input, current, opts, current)
	if _, err := fmt.Fprintln(stdout, line); err != nil {
		return err
	}

	// Arming spawns a process / reads the console, so do it AFTER the line is
	// printed. The no-reset fallback in particular calls ReadScreen, which frees
	// our own console — the status line must already be out the door.
	if armed != nil {
		ensureWatcher(opts, *armed.ResetsAt)
	} else if !hasReset(input.RateLimits) {
		armFromOwnScreen(opts)
	}
	return nil
}

// logTick sweeps stale locks on every status-line invocation, and in -debug mode
// also records a diagnostic trace (whether we can resolve our own claude PID and
// what usage/reset the JSON reported) — how we tell whether an instance that
// never armed was simply never called while over its threshold. That trace is
// the noisiest line in the log, so normal mode omits it.
func logTick(opts statusOptions, input statusInput) {
	if opts.stateDir == "" {
		return
	}
	sweepStaleLocks(opts.stateDir) // cleanup runs in every mode
	if !debugLog {
		return // the per-tick trace below is debug-only
	}
	claude := "FIND-FAILED"
	if pid, err := coninject.FindClaudePID(); err == nil {
		claude = "pid=" + strconv.FormatUint(uint64(pid), 10)
	}
	usage, reset := "no-rate-limits", "-"
	if input.RateLimits != nil && input.RateLimits.FiveHour != nil {
		fh := input.RateLimits.FiveHour
		if fh.UsedPercentage != nil {
			usage = fmt.Sprintf("%.0f%%", *fh.UsedPercentage)
		}
		if fh.ResetsAt != nil {
			reset = time.Unix(*fh.ResetsAt, 0).Format("15:04")
		}
	}
	logEvent(opts.stateDir, "tick claude=%s usage=%s reset=%s", claude, usage, reset)
}

func formatStatusLine(input statusInput, current time.Time, opts statusOptions, now time.Time) (string, *rateLimit) {
	effort := "--"
	if input.Effort != nil {
		effort = valueOrDash(input.Effort.Level)
	}
	modelParts := []string{valueOrDash(input.Model.DisplayName), effort}
	if input.Thinking != nil && input.Thinking.Enabled {
		modelParts = append(modelParts, "thinking")
	}

	context := "--%"
	if input.ContextWindow != nil {
		context = formatPercentage(input.ContextWindow.UsedPercentage)
	}

	parts := []string{
		strings.Join(modelParts, " "),
		"context " + context,
		formatRateLimit("usage", fiveHour(input.RateLimits), current),
	}
	if input.RateLimits != nil && input.RateLimits.SevenDay != nil {
		parts = append(parts, formatRateLimit("week", input.RateLimits.SevenDay, current))
	}

	armed := selectArm(input.RateLimits, opts.usageThreshold, opts.weekThreshold, now)
	if armed != nil {
		fireAt := time.Unix(*armed.ResetsAt, 0).In(current.Location()).Add(opts.postResetDelay)
		parts = append(parts, armWord+" "+formatLocalMinute(fireAt, current))
	}

	return strings.Join(parts, " | "), armed
}

// hasReset reports whether the JSON carried a 5-hour reset time. Without it the
// status line has no fire time to arm a watcher on, so it falls back to reading
// the reset off the screen (armFromOwnScreen).
func hasReset(limits *rateLimits) bool {
	return limits != nil && limits.FiveHour != nil && limits.FiveHour.ResetsAt != nil
}

// selectArm returns the rate limit that should arm a watcher: any limit at or
// above its threshold whose reset is still in the future, preferring the one
// whose reset is later.
//
// The future-reset guard is essential: right after a session is unblocked,
// Claude Code's status JSON briefly still reports the old over-limit usage with
// the just-elapsed reset. Arming on that would spawn a watcher whose fire time
// is already in the past, so it fires instantly, exits, frees the lock, and the
// next tick (still stale) arms another — a tight re-arm loop that spams the
// recovery prompt. A past reset is never something to wait for; the explicit
// `check` command, not the status line, handles already-elapsed resets.
func selectArm(limits *rateLimits, usageThreshold, weekThreshold float64, now time.Time) *rateLimit {
	if limits == nil {
		return nil
	}
	var selected *rateLimit
	for _, c := range []struct {
		limit     *rateLimit
		threshold float64
	}{
		{limits.FiveHour, usageThreshold},
		{limits.SevenDay, weekThreshold},
	} {
		l := c.limit
		if l == nil || l.UsedPercentage == nil || l.ResetsAt == nil || *l.UsedPercentage < c.threshold {
			continue
		}
		if *l.ResetsAt <= now.Unix() {
			continue // reset already passed: stale post-unblock data, nothing to wait for
		}
		if selected == nil || *l.ResetsAt > *selected.ResetsAt {
			selected = l
		}
	}
	return selected
}

// ensureWatcher spawns a detached watcher for the given reset, unless one is
// already armed for the same reset. Failures are silent: the status line must
// never block or error out over arming.
func ensureWatcher(opts statusOptions, resetUnix int64) {
	if opts.stateDir == "" {
		return
	}
	_ = os.MkdirAll(opts.stateDir, 0o755)

	// Lock per claude instance, not globally: several Claude Code sessions share
	// the same account-wide reset, so a single shared lock would let only the
	// first arm a watcher. Each status line owns its own claude's PID.
	claudePID, err := coninject.FindClaudePID()
	if err != nil {
		logDebug(opts.stateDir, "statusline FindClaudePID failed: %v", err)
		return
	}
	lockPath := filepath.Join(opts.stateDir, lockName(claudePID))

	existing, alive := readLock(lockPath)
	if existing == resetUnix && alive {
		logDebug(opts.stateDir, "statusline pid=%d danger-zone, already armed (reset=%d) -> skip", claudePID, existing)
		return
	}
	// Remove a real stale lock (different reset or dead watcher) so the atomic
	// claim can win. An empty in-progress claim (existing==0) is left alone so a
	// concurrent invocation cannot double-spawn.
	if existing != 0 {
		_ = os.Remove(lockPath)
	}

	// Atomically claim the right to spawn. If another status-line invocation is
	// already mid-claim, O_EXCL fails and we stand down — this kills the
	// double-spawn race seen in the field.
	claim, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		logDebug(opts.stateDir, "statusline pid=%d danger-zone, lost claim to concurrent invocation -> skip", claudePID)
		return
	}
	defer claim.Close()

	exe, err := os.Executable()
	if err != nil {
		return
	}

	cmd := exec.Command(exe, watchArgs(
		"-pid", strconv.FormatUint(uint64(claudePID), 10),
		"-reset", strconv.FormatInt(resetUnix, 10),
		"-delay", opts.postResetDelay.String(),
		"-state", opts.stateDir,
	)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNoWindow}
	if err := cmd.Start(); err != nil {
		logEvent(opts.stateDir, "statusline pid=%d spawn failed: %v", claudePID, err)
		return
	}
	fmt.Fprintf(claim, "%d %d\n", resetUnix, cmd.Process.Pid)
	logEvent(opts.stateDir, "statusline pid=%d armed watcher wpid=%d reset=%d", claudePID, cmd.Process.Pid, resetUnix)
	_ = cmd.Process.Release()
}

// armFromOwnScreen is the fallback when the status JSON carries no reset time.
// From the JSON alone we cannot tell whether this session is simply mid-call
// (the next tick will carry the data) or already blocked with no further usable
// JSON coming. So read our own claude's console ONCE and let the screen decide:
//
//   - modal with a parseable reset -> arm a watcher for it (the same sub-actions
//     `check` runs: ReadScreen, classifyForCheck, armWatcher);
//   - anything else -> not blocked, do nothing (the next tick brings the JSON).
//
// No polling: a single read suffices, because if this session were blocked the
// modal (and its "resets 12:40am") is already on screen right now. It must run
// only AFTER the status line has been printed — ReadScreen frees our console.
func armFromOwnScreen(opts statusOptions) {
	if opts.stateDir == "" {
		return
	}
	pid, err := coninject.FindClaudePID()
	if err != nil {
		logDebug(opts.stateDir, "fallback FindClaudePID failed: %v", err)
		return
	}
	// A live watcher already owns this claude: nothing to do — and skipping here
	// avoids re-reading the screen on every no-reset tick once we have armed.
	if _, alive := readLock(filepath.Join(opts.stateDir, lockName(pid))); alive {
		return
	}
	screen, err := coninject.ReadScreen(pid)
	if err != nil {
		logDebug(opts.stateDir, "fallback pid=%d ReadScreen failed: %v", pid, err)
		return
	}
	resetAt, raw, outcome := classifyForCheck(screen, time.Now())
	switch outcome {
	case outcomeArm:
		exe, err := os.Executable()
		if err != nil {
			return
		}
		armWatcher(opts.stateDir, exe, pid, resetAt.Unix(), opts.postResetDelay)
	case outcomeUnparseable:
		logEvent(opts.stateDir, "fallback pid=%d blocked but reset unparseable (raw=%q)", pid, raw)
	case outcomeSkipNotModal:
		// not blocked — the JSON just lacked a reset this tick; nothing to do
	}
}

// debugLog gates verbose, high-frequency logging (every status tick, every
// watcher poll, every skip). Off by default so the live watch.log records only
// actions; `continue-claude -debug` turns the full trace back on and propagates
// it to every spawned watcher (see watchArgs).
var debugLog bool

// logDebug logs only in -debug mode; action lines use logEvent (always on).
func logDebug(stateDir, format string, a ...any) {
	if !debugLog {
		return
	}
	logEvent(stateDir, format, a...)
}

// watchArgs prepends the "watch" subcommand and appends -debug when this process
// is in debug mode, so a spawned watcher inherits the same logging verbosity.
func watchArgs(flags ...string) []string {
	args := append([]string{"watch"}, flags...)
	if debugLog {
		args = append(args, "-debug")
	}
	return args
}

// logEvent appends a line to the shared watch.log so the status line's arming
// decisions sit in the same timeline as the watcher's actions.
func logEvent(stateDir, format string, a ...any) {
	if stateDir == "" {
		return
	}
	_ = os.MkdirAll(stateDir, 0o755)
	f, err := os.OpenFile(filepath.Join(stateDir, "watch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func readLock(path string) (resetUnix int64, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, false
	}
	resetUnix, _ = strconv.ParseInt(fields[0], 10, 64)
	pid, _ := strconv.ParseUint(fields[1], 10, 32)
	return resetUnix, coninject.IsAlive(uint32(pid))
}

// lockName is the per-claude-instance lock file name.
func lockName(claudePID uint32) string {
	return fmt.Sprintf("armed-%d.lock", claudePID)
}

// lockPID parses the claude PID out of a lock file name, or reports false if the
// name is not a lock file.
func lockPID(filename string) (uint32, bool) {
	if !strings.HasPrefix(filename, "armed-") || !strings.HasSuffix(filename, ".lock") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(filename, "armed-"), ".lock")
	pid, err := strconv.ParseUint(mid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(pid), true
}

// sweepStaleLocks removes armed-<pid>.lock files whose claude process no longer
// exists. Such a file is necessarily a leftover (the owning session is gone), so
// deleting it is safe and keeps the state directory from accumulating junk.
func sweepStaleLocks(stateDir string) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		pid, ok := lockPID(e.Name())
		if !ok {
			continue
		}
		if !coninject.IsAlive(pid) {
			_ = os.Remove(filepath.Join(stateDir, e.Name()))
		}
	}
}

func defaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".continue-claude"), nil
}

func fiveHour(limits *rateLimits) *rateLimit {
	if limits == nil {
		return nil
	}
	return limits.FiveHour
}

func formatRateLimit(label string, limit *rateLimit, current time.Time) string {
	usage, reset := "--%", "--"
	if limit != nil {
		usage = formatPercentage(limit.UsedPercentage)
		reset = formatReset(limit.ResetsAt, current)
	}
	return fmt.Sprintf("%s %s reset %s", label, usage, reset)
}

func valueOrDash(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return "--"
	}
	return value
}

func formatPercentage(value *float64) string {
	if value == nil {
		return "--%"
	}
	return fmt.Sprintf("%.0f%%", math.Round(*value))
}

func formatReset(value *int64, current time.Time) string {
	if value == nil {
		return "--"
	}
	return formatLocalMinute(time.Unix(*value, 0).In(current.Location()), current)
}

func formatLocalMinute(value, current time.Time) string {
	timePart := fmt.Sprintf("%d:%02d", value.Hour(), value.Minute())
	if sameLocalDate(value, current) {
		return timePart
	}
	return fmt.Sprintf("%d/%d %s", value.Month(), value.Day(), timePart)
}

func sameLocalDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
