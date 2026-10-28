// resmon samples a Mac's memory, CPU, disk, GPU, thermal state and UI-thread
// scheduling latency, then explains which resource (if any) is the bottleneck
// and which apps are responsible.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	dur := flag.Duration("d", 15*time.Second, "how long to sample before reporting")
	top := flag.Int("n", 12, "rows to show in the app/process tables")
	watch := flag.Bool("watch", false, "monitor continuously, one line per interval, until Ctrl-C")
	interval := flag.Duration("i", 5*time.Second, "sampling interval for -watch")
	asJSON := flag.Bool("json", false, "print the full report as JSON")
	noColor := flag.Bool("no-color", false, "disable colored output")
	ai := flag.Bool("ai", false, "also have an OpenAI model analyze the report (sends the report text to OpenAI; needs OPENAI_API_KEY)")
	model := flag.String("model", cmp.Or(os.Getenv("OPENAI_MODEL"), defaultAIModel), "OpenAI model for -ai (or set OPENAI_MODEL)")
	ask := flag.String("ask", "", "symptoms or a question to pass to the AI along with the report (implies -ai)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: resmon [flags]\n\nFinds resource bottlenecks (RAM, CPU, disk, GPU, heat) that make a Mac feel laggy.\nRun with sudo to include full detail for system processes.\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment: OPENAI_API_KEY (required for -ai), OPENAI_MODEL, OPENAI_BASE_URL (default https://api.openai.com/v1)\n")
	}
	flag.Parse()
	*ai = *ai || *ask != ""
	if *ai && *watch {
		fatal("-ai can't be combined with -watch")
	}
	if *ai && os.Getenv("OPENAI_API_KEY") == "" {
		fatal("-ai needs OPENAI_API_KEY set in the environment")
	}

	st := style{on: !*noColor && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *watch {
		runWatch(ctx, os.Stdout, *interval, st)
		return
	}

	r := collect(ctx, *dur, isTerminal(os.Stderr))
	stop() // Ctrl-C during sampling just ends it early; a new handler covers the AI request
	ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !*asJSON {
		printReport(os.Stdout, r, *top, st)
	}
	var aiErr error
	if *ai {
		var plain bytes.Buffer
		printReport(&plain, r, max(*top, 15), style{})
		if isTerminal(os.Stderr) {
			fmt.Fprintf(os.Stderr, "Asking %s for its analysis…", *model)
		}
		r.AIAnalysis, aiErr = aiAnalyze(ctx, *model, plain.String(), *ask)
		if isTerminal(os.Stderr) {
			fmt.Fprint(os.Stderr, "\r\x1b[K")
		}
		if aiErr == nil && !*asJSON {
			printAI(os.Stdout, *model, r.AIAnalysis, st)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fatal(err.Error())
		}
	}
	if aiErr != nil {
		fatal("AI analysis failed: " + aiErr.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "resmon: "+msg)
	os.Exit(1)
}

// collect samples system counters every second for dur (or until Ctrl-C),
// with process snapshots at both ends and the latency probe running throughout.
func collect(ctx context.Context, dur time.Duration, progress bool) *Report {
	info := readSystemInfo()
	probe := startLatencyProbe(time.Millisecond)
	t0 := time.Now()
	p0 := sampleProcs()
	ticks := []SysSample{sampleSystem()}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(dur)
loop:
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
		}
		ticks = append(ticks, sampleSystem())
		if progress {
			fmt.Fprintf(os.Stderr, "\rSampling… %2.0fs / %.0fs (Ctrl-C to stop early)", time.Since(t0).Seconds(), dur.Seconds())
		}
	}
	if len(ticks) < 2 {
		ticks = append(ticks, sampleSystem())
	}
	p1 := sampleProcs()
	procSecs := time.Since(t0).Seconds()
	lat := probe.stopAndDrain()
	if progress {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
	return buildReport(info, ticks, p0, p1, procSecs, lat)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
