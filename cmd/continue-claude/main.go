package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
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

Capture what every session currently shows on screen (debugging aid):
  continue-claude snapshot            (dump every claude.exe to snap-<PID>.txt)

Internal subcommands (the status line spawns these itself):
  continue-claude watch [-pid <PID>] [-reset <unix>] [-await-block] [-delay <dur>] [-state <dir>] [HH:MM]
  continue-claude snapshot [-pid <PID>] [-delay <dur>] [-out <file>]

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
	pid := fs.Uint("pid", 0, "target console process PID (0 = dump every claude.exe to snap-<pid>.txt)")
	delay := fs.Duration("delay", 0, "sleep before reading")
	out := fs.String("out", "", "write screen text to this file (default snap-<pid>.txt)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *pid == 0 {
		return snapshotAll(*delay)
	}

	if *delay > 0 {
		time.Sleep(*delay)
	}
	screen, err := coninject.ReadScreen(uint32(*pid))
	body := screen
	if err != nil {
		body = "ERROR: " + err.Error() + "\n"
	}
	dest := *out
	if dest == "" {
		dest = fmt.Sprintf("snap-%d.txt", *pid)
	}
	os.WriteFile(dest, []byte(body), 0o644)
	return err
}

// snapshotAll is the no-argument mode: scan every running claude.exe and dump
// each one's screen to snap-<pid>.txt in the current directory, so a single bare
// `continue-claude snapshot` captures all sessions at once. Each read is done in
// a child process (snapshotViaChild) — ReadScreen frees the console to attach to
// a target, which would tear down our own stdout mid-sweep if done in-process.
// Per-session read errors are recorded (in the file and on stderr) but do not
// fail the whole run; only an inability to enumerate processes does.
func snapshotAll(delay time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pids, err := coninject.ListClaudePIDs()
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		fmt.Fprintln(os.Stderr, "snapshot: no claude.exe found")
		return nil
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })

	if delay > 0 {
		time.Sleep(delay)
	}
	for _, p := range pids {
		dest := fmt.Sprintf("snap-%d.txt", p)
		screen, rerr := snapshotViaChild(exe, p)
		body := screen
		if rerr != nil {
			body = "ERROR: " + rerr.Error() + "\n"
		}
		if werr := os.WriteFile(dest, []byte(body), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "snapshot: pid=%d write %s failed: %v\n", p, dest, werr)
			continue
		}
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "snapshot: pid=%d -> %s (read error: %v)\n", p, dest, rerr)
		} else {
			fmt.Fprintf(os.Stderr, "snapshot: pid=%d -> %s\n", p, dest)
		}
	}
	return nil
}
