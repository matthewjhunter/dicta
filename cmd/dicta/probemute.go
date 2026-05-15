package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
	"github.com/matthewjhunter/dicta/internal/mute/pipewire"
)

// runProbeMute runs all mute.Source implementations side by side for
// a fixed window and reports what each observed. Diagnostic only —
// does not touch the daemon, does not enable --unmute-to-dictate.
//
// Exit code: 0 if at least one source observed a real transition;
// 1 if none did (likely means none of the built-in sources work for
// the mic, or no button was pressed during the window).
func runProbeMute(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("probe-mute", flag.ContinueOnError)
	fs.SetOutput(stderr)
	device := fs.String("device", "", "PipeWire node name or substring (e.g. \"AC-44\"); empty = default capture source")
	seconds := fs.Int("seconds", 15, "duration of the probe in seconds")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: dicta probe-mute [--device <name>] [--seconds <n>]\n\n")
		fmt.Fprintf(stderr, "Diagnostic: runs every built-in mute.Source side by side on the configured mic\n")
		fmt.Fprintf(stderr, "and reports what each saw. Toggle your mic's mute during the probe window.\n\n")
		fmt.Fprintf(stderr, "Note: pcm-zero is NOT exercised here. Reproducing it requires the audio pump,\n")
		fmt.Fprintf(stderr, "which only the daemon runs. To validate pcm-zero, enable --unmute-to-dictate\n")
		fmt.Fprintf(stderr, "--unmute-source=pcm-zero and watch dictad's logs.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *seconds < 1 {
		fmt.Fprintln(stderr, "dicta probe-mute: --seconds must be >= 1")
		return 2
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// In the daemon the pcm-zero source consumes PCM frames from
	// audioMonitor; replicating that here would require spawning the
	// whole capture pipeline. For probe-mute we keep scope to sources
	// that work standalone (currently: pipewire). The Usage message
	// calls this out so users running probe-mute on an AC-44 know
	// what to expect.
	sources := []mute.Source{
		pipewire.NewSource(logger, *device),
	}

	dur := time.Duration(*seconds) * time.Second
	fmt.Fprintf(stdout, "probe-mute: running %d source(s) for %s on device=%q\n",
		len(sources), dur, *device)
	fmt.Fprintf(stdout, "probe-mute: toggle your mic's mute button now\n\n")

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	sigCtx, sigCancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer sigCancel()

	type tally struct {
		name        string
		initial     mute.State
		seenInitial bool
		transitions int
		startErr    error
	}
	tallies := make([]tally, len(sources))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	start := time.Now()

	for i, s := range sources {
		tallies[i].name = s.Name()
		ch, err := s.Watch(sigCtx)
		if err != nil {
			mu.Lock()
			tallies[i].startErr = err
			mu.Unlock()
			fmt.Fprintf(stdout, "[%.2fs] %s: ERROR starting: %v\n", time.Since(start).Seconds(), s.Name(), err)
			continue
		}
		// Tell the user exactly what the source matched, so it's
		// obvious whether the hint resolved to the intended node.
		if pw, ok := s.(*pipewire.Source); ok {
			if id, name := pw.Resolved(); name != "" {
				fmt.Fprintf(stdout, "[%.2fs] %s: watching node %d %q\n",
					time.Since(start).Seconds(), s.Name(), id, name)
			}
		}
		wg.Go(func() {
			for ev := range ch {
				mu.Lock()
				if ev.Initial {
					tallies[i].initial = ev.State
					tallies[i].seenInitial = true
				} else {
					tallies[i].transitions++
				}
				mu.Unlock()
				if ev.Initial {
					fmt.Fprintf(stdout, "[%.2fs] %s: initial=%s\n",
						time.Since(start).Seconds(), ev.Source, ev.State)
				} else {
					fmt.Fprintf(stdout, "[%.2fs] %s: transition → %s\n",
						time.Since(start).Seconds(), ev.Source, ev.State)
				}
			}
		})
	}

	<-sigCtx.Done()
	wg.Wait()

	fmt.Fprintf(stdout, "\nprobe-mute: %s elapsed\n\nresults:\n", dur)
	worked := false
	for _, tally := range tallies {
		if tally.startErr != nil {
			fmt.Fprintf(stdout, "  %-10s : start error: %v\n", tally.name, tally.startErr)
			continue
		}
		mark := "no transitions"
		if tally.transitions > 0 {
			mark = fmt.Sprintf("%d transition(s)  RECOMMENDED", tally.transitions)
			worked = true
		}
		initial := "-"
		if tally.seenInitial {
			initial = tally.initial.String()
		}
		fmt.Fprintf(stdout, "  %-10s : initial=%-8s %s\n", tally.name, initial, mark)
	}

	if !worked {
		fmt.Fprintf(stdout, "\nno transitions observed. Either no button was pressed during the window,\n"+
			"or none of the probed sources work for this mic. For mics like the MXL AC-44 TAP whose\n"+
			"mute only surfaces via all-zero PCM, run dictad with --unmute-to-dictate --unmute-source=pcm-zero\n"+
			"--audio-monitor and watch the daemon log.\n")
		return 1
	}
	return 0
}
