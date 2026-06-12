package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/caridina-ai/continue-claude/internal/coninject"
)

const usage = `continue-claude — self-unblocking Claude Code via console keystroke injection

Usage:
  continue-claude [-state <dir>] [-usage-threshold N] [-week-threshold N] [-post-reset-delay <dur>]
                  (default: status line — reads Claude Code's status JSON on stdin)

Internal subcommands (the status line spawns these itself):
  continue-claude watch -pid <PID> -reset <unix> [-delay <dur>] [-state <dir>]
  continue-claude inject -pid <PID> [-delay <dur>] -mode <raw|unlock> [-text <s>] [-enter]
  continue-claude snapshot -pid <PID> [-delay <dur>] -out <file>

As the status line it prints the line and arms a watcher when usage crosses a
threshold. The watcher sleeps until the reset, then reads the target console and
either selects "Stop and wait" + continue (rate-limit menu), nudges an idle
prompt, or stands down (still busy).`

func main() {
	args := os.Args[1:]

	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Fprintln(os.Stderr, usage)
			return
		case "watch":
			fail("watch", runWatch(args[1:]))
			return
		case "inject":
			fail("inject", runInject(args[1:]))
			return
		case "snapshot":
			fail("snapshot", runSnapshot(args[1:]))
			return
		case "statusline":
			// Accept an explicit subcommand too, but it is optional.
			args = args[1:]
		}
	}

	fail("statusline", runStatusline(args, os.Stdin, os.Stdout, os.Stderr))
}

func fail(name string, err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	pid := fs.Uint("pid", 0, "target console process PID")
	delay := fs.Duration("delay", 0, "sleep before injecting")
	mode := fs.String("mode", "raw", "raw|unlock")
	text := fs.String("text", "", "text to type in raw mode")
	enter := fs.Bool("enter", false, "press Enter after text in raw mode")
	logPath := fs.String("log", "", "append a result line to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pid == 0 {
		return fmt.Errorf("missing -pid")
	}

	if *delay > 0 {
		time.Sleep(*delay)
	}

	var steps []coninject.Step
	switch *mode {
	case "raw":
		steps = append(steps, coninject.Text(*text))
		if *enter {
			steps = append(steps, coninject.Enter())
		}
	case "unlock":
		steps = []coninject.Step{
			coninject.Text("1"),
			coninject.Delay(400 * time.Millisecond),
			coninject.Text("continue"),
			coninject.Delay(150 * time.Millisecond),
			coninject.Enter(),
		}
	default:
		return fmt.Errorf("unknown -mode %q", *mode)
	}

	err := coninject.Inject(uint32(*pid), steps)
	if *logPath != "" {
		writeResult(*logPath, *pid, *mode, err)
	}
	return err
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

func writeResult(path string, pid uint, mode string, injErr error) {
	status := "success=1"
	if injErr != nil {
		status = "success=0 err=" + injErr.Error()
	}
	line := fmt.Sprintf("%s\tpid=%d mode=%s %s\n",
		time.Now().Format("2006-01-02 15:04:05"), pid, mode, status)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}
