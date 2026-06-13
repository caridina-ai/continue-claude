package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fp(f float64) *float64 { return &f }
func ip(i int64) *int64     { return &i }

func TestFormatStatusLineArmed(t *testing.T) {
	current := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	five := time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC).Unix()
	week := time.Date(2026, 6, 17, 5, 0, 0, 0, time.UTC).Unix()

	input := statusInput{
		RateLimits: &rateLimits{
			FiveHour: &rateLimit{UsedPercentage: fp(96), ResetsAt: ip(five)},
			SevenDay: &rateLimit{UsedPercentage: fp(10), ResetsAt: ip(week)},
		},
	}
	input.Model.DisplayName = "Fable 5"
	input.Effort = &struct {
		Level string `json:"level"`
	}{Level: "max"}
	input.Thinking = &struct {
		Enabled bool `json:"enabled"`
	}{Enabled: true}
	input.ContextWindow = &struct {
		UsedPercentage *float64 `json:"used_percentage"`
	}{UsedPercentage: fp(11)}

	// formatStatusLine is pure now: it computes the line and reports which limit
	// armed, but spawns nothing (runStatusline arms after printing).
	opts := statusOptions{usageThreshold: 90, weekThreshold: 95, postResetDelay: 3 * time.Minute}

	got, armed := formatStatusLine(input, current, opts, current)
	if armed == nil {
		t.Fatal("expected the five-hour limit to arm")
	}
	want := "Fable 5 max thinking | context 11% | usage 96% reset 14:30 | week 10% reset 6/17 5:00 | check 14:33"
	if got != want {
		t.Fatalf("formatStatusLine =\n  %q\nwant\n  %q", got, want)
	}
}

func TestFormatStatusLineNotArmed(t *testing.T) {
	current := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	five := time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC).Unix()

	input := statusInput{
		RateLimits: &rateLimits{
			FiveHour: &rateLimit{UsedPercentage: fp(50), ResetsAt: ip(five)},
		},
	}
	input.Model.DisplayName = "Fable 5"

	opts := statusOptions{usageThreshold: 90, weekThreshold: 95, postResetDelay: 3 * time.Minute}
	got, armed := formatStatusLine(input, current, opts, current)
	if armed != nil {
		t.Fatal("50% should not arm")
	}
	want := "Fable 5 -- | context --% | usage 50% reset 14:30"
	if got != want {
		t.Fatalf("formatStatusLine =\n  %q\nwant\n  %q", got, want)
	}
}

func TestSelectArm(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC)
	earlier := now.Add(1 * time.Hour).Unix() // future, sooner
	later := now.Add(3 * time.Hour).Unix()   // future, later
	past := now.Add(-2 * time.Hour).Unix()   // already elapsed

	t.Run("nil", func(t *testing.T) {
		if selectArm(nil, 90, 95, now) != nil {
			t.Fatal("nil limits should not arm")
		}
	})
	t.Run("below thresholds", func(t *testing.T) {
		l := &rateLimits{FiveHour: &rateLimit{UsedPercentage: fp(89), ResetsAt: ip(earlier)}}
		if selectArm(l, 90, 95, now) != nil {
			t.Fatal("89%% below 90 should not arm")
		}
	})
	t.Run("five hour over (102%)", func(t *testing.T) {
		l := &rateLimits{FiveHour: &rateLimit{UsedPercentage: fp(102), ResetsAt: ip(earlier)}}
		got := selectArm(l, 90, 95, now)
		if got == nil || *got.ResetsAt != earlier {
			t.Fatalf("expected five-hour armed at %d, got %v", earlier, got)
		}
	})
	t.Run("prefers later reset", func(t *testing.T) {
		l := &rateLimits{
			FiveHour: &rateLimit{UsedPercentage: fp(96), ResetsAt: ip(earlier)},
			SevenDay: &rateLimit{UsedPercentage: fp(99), ResetsAt: ip(later)},
		}
		got := selectArm(l, 90, 95, now)
		if got == nil || *got.ResetsAt != later {
			t.Fatalf("expected the later reset %d, got %v", later, got)
		}
	})
	t.Run("over threshold but reset already elapsed -> not armed", func(t *testing.T) {
		// The post-unblock stale-JSON case that caused the re-arm loop: usage still
		// reads over the limit while its reset is already in the past.
		l := &rateLimits{FiveHour: &rateLimit{UsedPercentage: fp(104), ResetsAt: ip(past)}}
		if got := selectArm(l, 90, 95, now); got != nil {
			t.Fatalf("a past reset must not arm, got %v", got)
		}
	})
	t.Run("five-hour elapsed but week still ahead -> arm week", func(t *testing.T) {
		l := &rateLimits{
			FiveHour: &rateLimit{UsedPercentage: fp(104), ResetsAt: ip(past)},
			SevenDay: &rateLimit{UsedPercentage: fp(99), ResetsAt: ip(later)},
		}
		got := selectArm(l, 90, 95, now)
		if got == nil || *got.ResetsAt != later {
			t.Fatalf("expected the still-future week reset %d, got %v", later, got)
		}
	})
}

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "armed.lock")

	// Write a lock the way ensureWatcher does ("<reset> <watcher-pid>\n") and read
	// it back. PID 2 may or may not be alive, so assert only that the reset parses.
	if err := os.WriteFile(path, []byte("1234567890 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reset, _ := readLock(path); reset != 1234567890 {
		t.Fatalf("readLock reset = %d, want 1234567890", reset)
	}

	if reset, alive := readLock(filepath.Join(t.TempDir(), "missing.lock")); reset != 0 || alive {
		t.Fatalf("missing lock should read (0,false), got (%d,%v)", reset, alive)
	}
}

func TestFormatLocalMinute(t *testing.T) {
	current := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	sameDay := time.Date(2026, 6, 12, 9, 5, 0, 0, time.UTC)
	otherDay := time.Date(2026, 6, 17, 5, 0, 0, 0, time.UTC)

	if got := formatLocalMinute(sameDay, current); got != "9:05" {
		t.Fatalf("same-day = %q, want 9:05", got)
	}
	if got := formatLocalMinute(otherDay, current); got != "6/17 5:00" {
		t.Fatalf("other-day = %q, want 6/17 5:00", got)
	}
}

func TestLockPID(t *testing.T) {
	if pid, ok := lockPID("armed-1234.lock"); !ok || pid != 1234 {
		t.Fatalf("armed-1234.lock => (%d,%v), want (1234,true)", pid, ok)
	}
	for _, bad := range []string{"armed.lock", "watch.log", "armed-.lock", "armed-abc.lock", "armed-1234.txt"} {
		if _, ok := lockPID(bad); ok {
			t.Fatalf("lockPID(%q) should be false", bad)
		}
	}
	// round-trip with lockName
	if pid, ok := lockPID(lockName(99)); !ok || pid != 99 {
		t.Fatalf("round-trip lockName(99) => (%d,%v)", pid, ok)
	}
}

func TestFormatPercentage(t *testing.T) {
	if got := formatPercentage(nil); got != "--%" {
		t.Fatalf("nil = %q, want --%%", got)
	}
	if got := formatPercentage(fp(95.6)); got != "96%" {
		t.Fatalf("95.6 = %q, want 96%%", got)
	}
}
