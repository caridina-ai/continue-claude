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
