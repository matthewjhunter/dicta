// Command dicta is the thin CLI client. One command per invocation; talks
// to dictad over $XDG_RUNTIME_DIR/dicta.sock (mode 0600).
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
	socketFlag := flag.String("socket", "", "control socket path (default: $XDG_RUNTIME_DIR/dicta.sock)")
	timeoutFlag := flag.Duration("timeout", 2*time.Second, "request timeout")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <command> [args...]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "commands:\n")
		fmt.Fprintf(os.Stderr, "  status                show daemon status\n")
		fmt.Fprintf(os.Stderr, "\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	socketPath := *socketFlag
	if socketPath == "" {
		p, err := control.DefaultSocketPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "dicta:", err)
			os.Exit(1)
		}
		socketPath = p
	}

	switch args[0] {
	case "status":
		os.Exit(runStatus(socketPath, *timeoutFlag))
	default:
		fmt.Fprintf(os.Stderr, "dicta: unknown command: %s\n", args[0])
		flag.Usage()
		os.Exit(2)
	}
}

func runStatus(socketPath string, timeout time.Duration) int {
	resp, err := control.Send(socketPath, control.Command{Cmd: "status"}, timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dicta:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		fmt.Fprintln(os.Stderr, "dicta:", err)
		return 1
	}
	if !resp.OK {
		return 1
	}
	return 0
}
