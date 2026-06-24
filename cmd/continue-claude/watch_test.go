package main

import (
	"testing"
	"time"
)

func TestParseClockTime(t *testing.T) {
	now := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)

	t.Run("later today", func(t *testing.T) {
		got, err := parseClockTime("19:30", now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 6, 12, 19, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("already past rolls to tomorrow", func(t *testing.T) {
		got, err := parseClockTime("9:05", now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 6, 13, 9, 5, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("with seconds", func(t *testing.T) {
		got, err := parseClockTime("19:30:45", now)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 6, 12, 19, 30, 45, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	for _, bad := range []string{"", "19", "25:00", "19:60", "ab:cd", "19:30:99"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			if _, err := parseClockTime(bad, now); err == nil {
				t.Fatalf("parseClockTime(%q) should have errored", bad)
			}
		})
	}
}

func TestParseScreenReset(t *testing.T) {
	// 00:28 local, like the real session that resets at 12:40am.
	now := time.Date(2026, 6, 13, 0, 28, 0, 0, time.UTC)
	screen := "> hello\n  You've hit your session limit · resets 12:40am (Asia/Taipei)\n  /upgrade ...\n"

	got, raw, ok := parseScreenReset(screen, now)
	if !ok {
		t.Fatalf("expected to parse, raw=%q", raw)
	}
	want := time.Date(2026, 6, 13, 0, 40, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s (raw=%q)", got, want, raw)
	}

	t.Run("afternoon", func(t *testing.T) {
		s := "resets 3:05pm (Asia/Taipei)"
		got, _, ok := parseScreenReset(s, now)
		if !ok || got.Hour() != 15 || got.Minute() != 5 {
			t.Fatalf("3:05pm -> %s ok=%v", got, ok)
		}
	})

	// 12-hour boundary cases: 12:xxam is just after midnight (00:xx), 12:xxpm is
	// just after noon (12:xx). Claude Code prints "12:40am" for 00:40, so getting
	// this wrong would arm 12 hours off.
	for _, c := range []struct {
		in   string
		h, m int
	}{
		{"resets 12:40am (Asia/Taipei)", 0, 40}, // just after midnight
		{"resets 12:05pm (Asia/Taipei)", 12, 5}, // just after noon
		{"resets 11:59pm (Asia/Taipei)", 23, 59},
		{"resets 1:00am (Asia/Taipei)", 1, 0},
		// On-the-hour resets are printed minute-less ("1am", not "1:00am"); these
		// must parse just the same, or a real block reads as unparseable.
		{"resets 1am (Asia/Taipei)", 1, 0},
		{"resets 3pm (Asia/Taipei)", 15, 0},
		{"resets 12am (Asia/Taipei)", 0, 0},  // midnight
		{"resets 12pm (Asia/Taipei)", 12, 0}, // noon
	} {
		t.Run(c.in, func(t *testing.T) {
			got, _, ok := parseScreenReset(c.in, now)
			if !ok || got.Hour() != c.h || got.Minute() != c.m {
				t.Fatalf("%q -> %02d:%02d ok=%v, want %02d:%02d", c.in, got.Hour(), got.Minute(), ok, c.h, c.m)
			}
		})
	}

	t.Run("no resets text", func(t *testing.T) {
		if _, _, ok := parseScreenReset("nothing here", now); ok {
			t.Fatal("should not parse")
		}
	})

	t.Run("ahead/behind disambiguation against the 5h window", func(t *testing.T) {
		cases := []struct {
			name string
			now  time.Time
			line string
			want time.Time
		}{
			{
				// Fresh block at 22:00, reset 0:40 is ~2.7h ahead -> pending tonight.
				"fresh block, reset still ahead",
				time.Date(2026, 6, 12, 22, 0, 0, 0, time.UTC),
				"You've hit your session limit · resets 12:40am",
				time.Date(2026, 6, 13, 0, 40, 0, 0, time.UTC),
			},
			{
				// Checked next afternoon: 0:40 rolled forward is ~10.7h ahead (>5h),
				// so it's really today's already-passed 0:40 -> act now.
				"stale session window, reset already passed",
				time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC),
				"You've hit your session limit · resets 12:40am",
				time.Date(2026, 6, 13, 0, 40, 0, 0, time.UTC),
			},
			{
				// 8am seeing "resets 8:00pm": 12h ahead (>5h) -> yesterday 20:00.
				"8am sees 8pm session -> yesterday",
				time.Date(2026, 6, 13, 8, 0, 0, 0, time.UTC),
				"You've hit your session limit · resets 8:00pm",
				time.Date(2026, 6, 12, 20, 0, 0, 0, time.UTC),
			},
			{
				// 15:00 seeing "resets 7:00pm": 4h ahead (<=5h) -> pending today.
				"afternoon session reset within window",
				time.Date(2026, 6, 13, 15, 0, 0, 0, time.UTC),
				"You've hit your session limit · resets 7:00pm",
				time.Date(2026, 6, 13, 19, 0, 0, 0, time.UTC),
			},
			{
				// Weekly limit at 11:00pm is ~9h ahead (>5h) but must NOT flip: the
				// weekly window is not bounded by 5h, so it's a genuine pending reset.
				"weekly limit far ahead is not flipped",
				time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC),
				"You've hit your weekly limit · resets 11:00pm",
				time.Date(2026, 6, 13, 23, 0, 0, 0, time.UTC),
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, raw, ok := parseScreenReset(c.line, c.now)
				if !ok {
					t.Fatalf("did not parse (raw=%q)", raw)
				}
				if !got.Equal(c.want) {
					t.Fatalf("got %s, want %s", got, c.want)
				}
			})
		}
	})

	t.Run("unknown cross-day form returns raw, not ok", func(t *testing.T) {
		_, raw, ok := parseScreenReset("resets 6/15 0:12am (Asia/Taipei)", now)
		if ok {
			t.Fatalf("cross-day should be unparseable for now, got ok (raw=%q)", raw)
		}
		if raw == "" {
			t.Fatal("expected raw text to be captured for diagnosis")
		}
	})
}
