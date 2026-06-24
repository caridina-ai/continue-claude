package coninject

import "testing"

const modalScreen = `●Some earlier assistant output sitting up in the scrollback.

▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔
   What do you want to do?

   > 1. Stop and wait for limit to reset
     2. Add funds to continue with usage credits
     3. Upgrade your plan
     4. Upgrade to Team plan

   Enter to confirm · Esc to cancel`

// blockedIdleScreen is the snap-5076 trap: the limit banner is on screen, but
// there is no menu — just the idle prompt. Pressing 1 here would be wrong, so it
// must classify as idle (nudge with the continue prompt), never modal.
const blockedIdleScreen = `> hello
You've hit your session limit · resets 3:30pm (Asia/Taipei)
/upgrade to increase your usage limit.

✻ Worked for 0s
────────────────────────────────────────────────────────────────
>
────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`

const busyScreen = `●Working on the thing...

  Running 1 shell command…

* Percolating… 2m 12s · ↓ 952k tokens
────────────────────────────────────────────────────────────────
>
────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

const idleScreen = `●All done.

✻ Brewed for 3m 5s
────────────────────────────────────────────────────────────────
>
────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   State
	}{
		{"modal", modalScreen, StateModal},
		{"blocked but idle (no menu)", blockedIdleScreen, StateUnknown},
		{"busy", busyScreen, StateUnknown},
		{"idle", idleScreen, StateUnknown},
		{"empty", "", StateUnknown},
		{"garbage", "just some\nrandom lines\nwith no markers", StateUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.screen); got != c.want {
				t.Fatalf("Classify(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   bool
	}{
		{"inline rejection at idle prompt", blockedIdleScreen, true},
		{"weekly limit wording", "> hi\nYou've hit your weekly limit · resets 6/20 3pm\n>\n", true},
		{"menu only (no inline line)", modalScreen, false},
		{"busy", busyScreen, false},
		{"idle and unblocked", idleScreen, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRateLimited(c.screen); got != c.want {
				t.Fatalf("IsRateLimited(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestClassifyIgnoresScrollback guards the reason we match only the bottom
// rows: a session that *quotes* the menu — including the exact "1. Stop and
// wait" option — up in the scrollback, but whose live region is an idle prompt,
// must NOT look like a modal. (The snap-5076 trap.)
func TestClassifyIgnoresScrollback(t *testing.T) {
	screen := "> show me the rate-limit menu\n" +
		"It was:\n" +
		"> 1. Stop and wait for limit to reset\n" +
		"  2. Upgrade your plan\n" +
		"  Enter to confirm · Esc to cancel\n" +
		"\n" + idleScreen
	if got := Classify(screen); got != StateUnknown {
		t.Fatalf("Classify with menu text in scrollback = %v, want %v", got, StateUnknown)
	}
}
