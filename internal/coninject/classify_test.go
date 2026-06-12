package coninject

import "testing"

const modalScreen = `●Some earlier assistant output that even mentions esc to interrupt in passing.

▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔
   What do you want to do?

   > 1. Stop and wait for limit to reset
     2. Add funds to continue with usage credits
     3. Upgrade your plan
     4. Upgrade to Team plan

   Enter to confirm · Esc to cancel`

const busyScreen = `●Working on the thing...

  Running 1 shell command…

* Percolating… 2m 12s · ↓ 952k tokens
────────────────────────────────────────────────────────────────
>
────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt`

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
		{"busy", busyScreen, StateBusy},
		{"idle", idleScreen, StateIdle},
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

// TestClassifyIgnoresScrollback guards the reason we only match the bottom
// rows: the phrase "esc to interrupt" appearing up in the transcript must not
// make an idle screen look busy.
func TestClassifyIgnoresScrollback(t *testing.T) {
	screen := "I once typed esc to interrupt in a message.\n" +
		"And also Stop and wait for limit to reset, just chatting.\n" + idleScreen
	if got := Classify(screen); got != StateIdle {
		t.Fatalf("Classify with scrollback noise = %v, want %v", got, StateIdle)
	}
}
