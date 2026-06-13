# continue-claude

A Claude Code **status line** for Windows that lets a session **unblock itself**
when it hits a usage rate limit — no human at the keyboard required.

When you run out of 5-hour (or weekly) quota, Claude Code stops at a modal:

```
What do you want to do?
> 1. Stop and wait for limit to reset
  2. Add funds to continue with usage credits
  3. Upgrade your plan
  4. Upgrade to Team plan
```

`continue-claude` notices you are about to be blocked, and once the limit
resets it reaches back into Claude Code's own console and presses `1`
(*Stop and wait*) followed by `continue` — so your work picks up from a fresh
0% window automatically.

## How it works

The status line is invoked by Claude Code roughly once a minute. Each call:

1. Prints the usual status line (model, context, usage, week).
2. If usage is in the danger zone (≥ 90% by default), it spawns a detached
   **watcher** and appends a `check HH:MM` marker to the status line.

The watcher sleeps until the reset time (plus a small delay), then reads the
target console's screen and classifies the live state:

| Screen shows                          | State | Action                                   |
| ------------------------------------- | ----- | ---------------------------------------- |
| `Stop and wait for limit to reset`    | modal | inject `1`, then `continue` + Enter      |
| `esc to interrupt`                    | busy  | stand down (it got past on its own)      |
| an empty prompt                       | idle  | inject a self-correcting recovery prompt |

### The injection trick

Keystrokes are delivered by attaching to Claude Code's **console input buffer**
(`AttachConsole` + `WriteConsoleInputW`) rather than the windowing layer. This
bypasses Windows UIPI, so it works even when Claude Code runs **elevated**
(as long as the elevated console was started by the same interactive user).
State is read back the same way via `CONOUT$` + `ReadConsoleOutputCharacterW`.

## Install

```sh
go install github.com/caridina-ai/continue-claude/cmd/continue-claude@latest
```

This drops `continue-claude.exe` into your Go bin (`$(go env GOPATH)\bin`),
which is normally already on `PATH`.

Then point Claude Code's status line at it in `settings.json`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "continue-claude"
  }
}
```

Restart Claude Code for the new status line to take effect.

## Configuration

The status line accepts these flags (append them after `continue-claude` in the
`command`):

| Flag                 | Default              | Meaning                                       |
| -------------------- | -------------------- | --------------------------------------------- |
| `-usage-threshold`   | `90`                 | 5-hour usage % that arms a watcher            |
| `-week-threshold`    | `95`                 | 7-day usage % that arms a watcher             |
| `-post-reset-delay`  | `3m`                 | wait after reset before the watcher checks    |
| `-state`             | `~/.continue-claude` | directory for the watcher lock and `watch.log` |
| `-debug`             | off                  | verbose logging (a switch, no value): every tick/poll/skip |

By default the status line and its watchers log only actions — arming,
injecting, stand-downs — to `~/.continue-claude/watch.log`. Add `-debug` (e.g.
`"command": "continue-claude -debug"`) for the full per-tick/per-poll trace.

Multiple Claude Code sessions are handled independently: each status line arms
and tracks its own watcher with a per-instance lock (`armed-<pid>.lock`), so all
of them recover when a shared account-wide limit resets.

## Unblock a stuck session manually

You don't have to wait for the status line to arm a watcher in advance. If
sessions are *already* frozen at the rate-limit modal, run `check` in another
terminal: it scans every running Claude Code, reads each blocked one's reset
time off its screen, and arms a watcher per session — no PID or time needed.

```sh
continue-claude check            # scan all sessions, arm a watcher per blocked one
continue-claude check -dry-run   # show what it would arm, without spawning
```

To act on a single session with a reset time you already know, use `watch`. It
auto-detects the blocked claude (no PID needed) and waits until the time you
give it, so you don't have to sit there:

```sh
continue-claude watch 19:30    # wait until 19:30, then press 1 + continue
continue-claude watch          # act now (e.g. the limit already reset)
```

- The time is a 24-hour `HH:MM`; if it has already passed today it is taken as
  tomorrow. A `-delay` (default `3m`) is added as a safety buffer after the
  reset — pass `-delay 0` to act exactly at the given time.
- With a single Claude Code running it is picked automatically. With several it
  does not guess — it lists each PID and its detected state and asks you to
  re-run with `-pid <PID>`.
- It reads the screen before acting, so if the session is actually busy it
  stands down instead of injecting.

## Caveats

- **Windows only.** It relies on the Win32 console API.
- The watcher reads the bottom of the screen to classify state; if Claude Code's
  prompt text changes substantially the match strings may need updating.
- The idle recovery prompt is injected as keystrokes, so if you happen to be
  typing in the prompt at that exact moment your input can get mixed in.

## Internal subcommands

The status line spawns the binary against itself; you do not normally call these
directly (though `watch` doubles as the manual unblock command above):

```
continue-claude watch    [-pid <PID>] [-reset <unix>] [-delay <dur>] [-state <dir>] [HH:MM]
continue-claude snapshot -pid <PID> [-delay <dur>] -out <file>
```

## Development

Built and validated with **Claude Opus 4.8**.
