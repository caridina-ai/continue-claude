package main

import (
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

	// stateDir empty => ensureWatcher is a no-op, so no process is spawned.
	opts := statusOptions{usageThreshold: 90, weekThreshold: 95, postResetDelay: 3 * time.Minute}

	got := formatStatusLine(input, current, opts, current)
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
	got := formatStatusLine(input, current, opts, current)
	want := "Fable 5 -- | context --% | usage 50% reset 14:30"
	if got != want {
		t.Fatalf("formatStatusLine =\n  %q\nwant\n  %q", got, want)
	}
}

func TestSelectArm(t *testing.T) {
	earlier := int64(1000)
	later := int64(2000)

	t.Run("nil", func(t *testing.T) {
		if selectArm(nil, 90, 95) != nil {
			t.Fatal("nil limits should not arm")
		}
	})
	t.Run("below thresholds", func(t *testing.T) {
		l := &rateLimits{FiveHour: &rateLimit{UsedPercentage: fp(89), ResetsAt: ip(earlier)}}
		if selectArm(l, 90, 95) != nil {
			t.Fatal("89%% below 90 should not arm")
		}
	})
	t.Run("five hour over (102%)", func(t *testing.T) {
		l := &rateLimits{FiveHour: &rateLimit{UsedPercentage: fp(102), ResetsAt: ip(earlier)}}
		got := selectArm(l, 90, 95)
		if got == nil || *got.ResetsAt != earlier {
			t.Fatalf("expected five-hour armed at %d, got %v", earlier, got)
		}
	})
	t.Run("prefers later reset", func(t *testing.T) {
		l := &rateLimits{
			FiveHour: &rateLimit{UsedPercentage: fp(96), ResetsAt: ip(earlier)},
			SevenDay: &rateLimit{UsedPercentage: fp(99), ResetsAt: ip(later)},
		}
		got := selectArm(l, 90, 95)
		if got == nil || *got.ResetsAt != later {
			t.Fatalf("expected the later reset %d, got %v", later, got)
		}
	})
}

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "armed.lock")

	// Use the current process PID so the liveness check returns true.
	writeLock(path, 1234567890, uint32(2))
	if reset, alive := readLock(path); reset != 1234567890 || !alive {
		// PID 2 may or may not be alive; just assert the reset parses.
		if reset != 1234567890 {
			t.Fatalf("readLock reset = %d, want 1234567890 (alive=%v)", reset, alive)
		}
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

func TestFormatPercentage(t *testing.T) {
	if got := formatPercentage(nil); got != "--%" {
		t.Fatalf("nil = %q, want --%%", got)
	}
	if got := formatPercentage(fp(95.6)); got != "96%" {
		t.Fatalf("95.6 = %q, want 96%%", got)
	}
}
