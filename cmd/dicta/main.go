// Command dicta is the thin CLI client. One command per invocation; talks
// to dictad over $XDG_RUNTIME_DIR/dicta.sock (mode 0600).
//
// Subcommands map directly to the v1 control protocol (§5.6):
//
//	dicta status
//	dicta toggle_talk --mode type|clip
//	dicta commit --text "..."
//	dicta cancel
//	dicta shutdown
//
// Per-subcommand flags are parsed via flag.NewFlagSet so each subcommand
// gets its own --help. The global --socket / --timeout flags must come
// before the subcommand.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/matthewjhunter/dicta/internal/control"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. Splitting it out from main lets
// tests drive the CLI with synthetic argv and capture stdout/stderr
// without process-level juggling.
func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("dicta", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socketFlag := fs.String("socket", "", "control socket path (default: $XDG_RUNTIME_DIR/dicta.sock)")
	timeoutFlag := fs.Duration("timeout", 2*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: dicta [flags] <command> [args...]\n\n")
		fmt.Fprintf(stderr, "commands:\n")
		fmt.Fprintf(stderr, "  status                       show daemon status\n")
		fmt.Fprintf(stderr, "  toggle_talk --mode type|clip toggle a dictation session\n")
		fmt.Fprintf(stderr, "  commit --text \"...\"          commit clip-mode buffer to clipboard\n")
		fmt.Fprintf(stderr, "  cancel                       cancel an open clip-mode session\n")
		fmt.Fprintf(stderr, "  shutdown                     ask the daemon to exit cleanly\n")
		fmt.Fprintf(stderr, "\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}

	socketPath := *socketFlag
	if socketPath == "" {
		p, err := control.DefaultSocketPath()
		if err != nil {
			fmt.Fprintln(stderr, "dicta:", err)
			return 1
		}
		socketPath = p
	}

	switch rest[0] {
	case "status":
		return runStatus(socketPath, *timeoutFlag, stdout, stderr)
	case "toggle_talk":
		return runToggle(socketPath, *timeoutFlag, rest[1:], stdout, stderr)
	case "commit":
		return runCommit(socketPath, *timeoutFlag, rest[1:], stdout, stderr)
	case "cancel":
		return runCancel(socketPath, *timeoutFlag, stdout, stderr)
	case "shutdown":
		return runShutdown(socketPath, *timeoutFlag, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dicta: unknown command: %s\n", rest[0])
		fs.Usage()
		return 2
	}
}

func runStatus(socketPath string, timeout time.Duration, stdout, stderr *os.File) int {
	return sendAndPrint(socketPath, control.Command{Cmd: "status"}, timeout, stdout, stderr)
}

func runToggle(socketPath string, timeout time.Duration, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("toggle_talk", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "session mode: type | clip")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "type" && *mode != "clip" {
		fmt.Fprintln(stderr, "dicta: toggle_talk requires --mode type|clip")
		return 2
	}
	return sendAndPrint(socketPath, control.Command{Cmd: "toggle_talk", Mode: *mode}, timeout, stdout, stderr)
}

func runCommit(socketPath string, timeout time.Duration, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	text := fs.String("text", "", "panel-edited text to send to wl-copy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Empty text is allowed by the protocol; the daemon will hand "" to
	// wl-copy. Emit a stderr note so the user notices an empty -text.
	if *text == "" {
		fmt.Fprintln(stderr, "dicta: warning: --text is empty; daemon will commit an empty clipboard")
	}
	return sendAndPrint(socketPath, control.Command{Cmd: "commit", Text: *text}, timeout, stdout, stderr)
}

func runCancel(socketPath string, timeout time.Duration, stdout, stderr *os.File) int {
	return sendAndPrint(socketPath, control.Command{Cmd: "cancel"}, timeout, stdout, stderr)
}

func runShutdown(socketPath string, timeout time.Duration, stdout, stderr *os.File) int {
	// `shutdown` returns not_implemented in v1 (the daemon exits via
	// SIGTERM, not a control-socket command). Surface that response
	// verbatim — silencing it would hide a real protocol gap.
	return sendAndPrint(socketPath, control.Command{Cmd: "shutdown"}, timeout, stdout, stderr)
}

// sendAndPrint dials the daemon, sends cmd, and prints the JSON
// response. Returns 0 on a Response with OK=true, 1 otherwise. Network
// failures and malformed responses also map to 1 with the error
// printed to stderr.
func sendAndPrint(socketPath string, cmd control.Command, timeout time.Duration, stdout, stderr *os.File) int {
	resp, err := control.Send(socketPath, cmd, timeout)
	if err != nil {
		fmt.Fprintln(stderr, "dicta:", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		fmt.Fprintln(stderr, "dicta:", err)
		return 1
	}
	if !resp.OK {
		return 1
	}
	return 0
}
