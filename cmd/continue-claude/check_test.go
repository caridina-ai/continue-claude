package main

import (
	"testing"
	"time"
)

func TestClassifyForCheck(t *testing.T) {
	// 00:28 local, like the real session that resets at 12:40am.
	now := time.Date(2026, 6, 13, 0, 28, 0, 0, time.UTC)

	t.Run("blocked with parseable reset -> arm", func(t *testing.T) {
		screen := "> hello\n" +
			"You've hit your session limit · resets 12:40am (Asia/Taipei)\n" +
			"What do you want to do?\n" +
			"> 1. Stop and wait for limit to reset\n" +
			"  2. Upgrade your plan\n" +
			"  Enter to confirm · Esc to cancel\n"
		resetAt, _, outcome := classifyForCheck(screen, now)
		if outcome != outcomeArm {
			t.Fatalf("outcome = %v, want arm", outcome)
		}
		want := time.Date(2026, 6, 13, 0, 40, 0, 0, time.UTC)
		if !resetAt.Equal(want) {
			t.Fatalf("reset = %s, want %s", resetAt, want)
		}
	})

	t.Run("idle session -> skip", func(t *testing.T) {
		screen := "✻ Brewed for 3m\n>\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
		if _, _, outcome := classifyForCheck(screen, now); outcome != outcomeSkipNotModal {
			t.Fatalf("outcome = %v, want skip", outcome)
		}
	})

	t.Run("blocked but unparseable reset -> unparseable+raw", func(t *testing.T) {
		screen := "You've hit your session limit · resets 6/15 0:12am (Asia/Taipei)\n" +
			"> 1. Stop and wait for limit to reset\n" +
			"  2. Upgrade your plan\n" +
			"  Enter to confirm · Esc to cancel\n"
		_, raw, outcome := classifyForCheck(screen, now)
		if outcome != outcomeUnparseable {
			t.Fatalf("outcome = %v, want unparseable", outcome)
		}
		if raw == "" {
			t.Fatal("expected raw reset text to be captured")
		}
	})
}
