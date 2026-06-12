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
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := statusOptions{usageThreshold: 90, weekThreshold: 95, postResetDelay: 3 * time.Minute}
	defaultState, _ := defaultStateDir()
	fs.StringVar(&opts.stateDir, "state", defaultState, "directory for armed-watcher state")
	fs.Float64Var(&opts.usageThreshold, "usage-threshold", opts.usageThreshold, "5-hour usage %% that arms a watcher")
	fs.Float64Var(&opts.weekThreshold, "week-threshold", opts.weekThreshold, "7-day usage %% that arms a watcher")
	fs.DurationVar(&opts.postResetDelay, "post-reset-delay", opts.postResetDelay, "delay after reset before the watcher checks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var input statusInput
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		return fmt.Errorf("read Claude Code status JSON: %w", err)
	}

	current := time.Now()
	line := formatStatusLine(input, current, opts, current)
	_, err := fmt.Fprintln(stdout, line)
	return err
}

func formatStatusLine(input statusInput, current time.Time, opts statusOptions, now time.Time) string {
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

	if armed := selectArm(input.RateLimits, opts.usageThreshold, opts.weekThreshold); armed != nil {
		fireAt := time.Unix(*armed.ResetsAt, 0).In(current.Location()).Add(opts.postResetDelay)
		ensureWatcher(opts, *armed.ResetsAt)
		parts = append(parts, armWord+" "+formatLocalMinute(fireAt, current))
	}

	return strings.Join(parts, " | ")
}

// selectArm returns the rate limit that should arm a watcher: any limit at or
// above its threshold, preferring the one whose reset is later.
func selectArm(limits *rateLimits, usageThreshold, weekThreshold float64) *rateLimit {
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
		return
	}
	lockPath := filepath.Join(opts.stateDir, lockName(claudePID))

	if existing, alive := readLock(lockPath); existing == resetUnix && alive {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	cmd := exec.Command(exe, "watch",
		"-pid", strconv.FormatUint(uint64(claudePID), 10),
		"-reset", strconv.FormatInt(resetUnix, 10),
		"-delay", opts.postResetDelay.String(),
		"-state", opts.stateDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNoWindow}
	if err := cmd.Start(); err != nil {
		return
	}
	writeLock(lockPath, resetUnix, uint32(cmd.Process.Pid))
	_ = cmd.Process.Release()
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

func writeLock(path string, resetUnix int64, pid uint32) {
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %d\n", resetUnix, pid)), 0o644)
}

// lockName is the per-claude-instance lock file name.
func lockName(claudePID uint32) string {
	return fmt.Sprintf("armed-%d.lock", claudePID)
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
