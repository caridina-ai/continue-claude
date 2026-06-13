package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/caridina-ai/continue-claude/internal/coninject"
)

const usage = `continue-claude — self-unblocking Claude Code via console keystroke injection

Usage:
  continue-claude [-state <dir>] [-usage-threshold N] [-week-threshold N]
                  [-post-reset-delay <dur>] [-debug]
                  (default: status line — reads Claude Code's status JSON on stdin)

Unblock stuck sessions. Both auto-detect the blocked claude(s) and wait until
the reset, then press 1 + continue:
  continue-claude check              (scan ALL claude sessions; arm one watcher
                                      per blocked session, reset read off-screen)
  continue-claude watch 19:30        (one session; wait until 19:30, then unlock)
  continue-claude watch              (one session, now — e.g. the limit reset)

Internal subcommands (the status line spawns these itself):
  continue-claude watch [-pid <PID>] [-reset <unix>] [-delay <dur>] [-state <dir>] [HH:MM]
  continue-claude snapshot -pid <PID> [-delay <dur>] -out <file>

As the status line it prints the line and arms a watcher when usage crosses a
threshold. The watcher sleeps until the reset, then reads the target console and
either selects "Stop and wait" + continue (rate-limit menu), nudges an idle
prompt, or stands down (still busy).

By default only actions and watcher outcomes are written to watch.log; -debug
adds the full per-tick and per-poll trace and is inherited by spawned watchers.`

func main() {
	args := os.Args[1:]

	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Fprintln(os.Stderr, usage)
			return
		case "version", "-version", "--version":
			fmt.Fprintln(os.Stdout, versionString())
			return
		case "check":
			fail("check", runCheck(args[1:]))
			return
		case "watch":
			fail("watch", runWatch(args[1:]))
			return
		case "snapshot":
			fail("snapshot", runSnapshot(args[1:]))
			return
		}
	}

	fail("continue-claude", runStatusline(args, os.Stdin, os.Stdout, os.Stderr))
}

// versionString reports the module version the binary was built from. When
// installed via `go install ...@vX.Y.Z` this is the git tag; for a local build
// it is "(devel)". Nothing to bump by hand — tag a release and it follows.
func versionString() string {
	v := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		v = info.Main.Version
	}
	return "continue-claude " + v
}

func fail(name string, err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

func runSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	pid := fs.Uint("pid", 0, "target console process PID")
	delay := fs.Duration("delay", 0, "sleep before reading")
	out := fs.String("out", "", "write screen text to this file (default stderr is gone after attach; use a file)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pid == 0 {
		return fmt.Errorf("missing -pid")
	}
	if *delay > 0 {
		time.Sleep(*delay)
	}

	screen, err := coninject.ReadScreen(uint32(*pid))
	if *out != "" {
		body := screen
		if err != nil {
			body = "ERROR: " + err.Error() + "\n"
		}
		os.WriteFile(*out, []byte(body), 0o644)
	}
	return err
}
