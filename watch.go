package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// runWatch prints one line per interval so intermittent lag can be matched to
// what the system was doing at that moment.
func runWatch(ctx context.Context, w io.Writer, interval time.Duration, st style) {
	probe := startLatencyProbe(5 * time.Millisecond)
	defer probe.stopAndDrain()

	prevS, prevP, prevT := sampleSystem(), sampleProcs(), time.Now()
	t := time.NewTicker(interval)
	defer t.Stop()

	header := st.bold(fmt.Sprintf("%-8s %5s %5s %-5s %7s %11s %7s %7s %11s %4s %12s  %s",
		"TIME", "CPU", "LOAD", "PRESS", "SWAP", "SWAPIN/OUT", "DECMP", "PAGEIN", "DISK R/W MB", "GPU", "LAG p99/max", "TOP CPU APP"))
	fmt.Fprintln(w, st.dim(fmt.Sprintf("Sampling every %s; rates are per second. Ctrl-C to stop.", interval)))

	type worst struct {
		at     time.Time
		lagMax float64
		desc   string
	}
	var (
		lines, alerts int
		worstLag      worst
	)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(w, "\n%d intervals, %d flagged.", lines, alerts)
			if worstLag.lagMax > 0 {
				fmt.Fprintf(w, " Worst UI-thread delay: %.0f ms at %s (%s).", worstLag.lagMax, worstLag.at.Format("15:04:05"), worstLag.desc)
			}
			fmt.Fprintln(w)
			return
		case <-t.C:
		}
		s, p, now := sampleSystem(), sampleProcs(), time.Now()
		lat := latencyStats(probe.drain())
		rt := ratesBetween(prevS, s)
		apps := aggregateApps(procUsages(prevP, p, now.Sub(prevT).Seconds()))
		topApp := slices.MaxFunc(apps, func(x, y AppUsage) int { return cmp.Compare(x.CPUPct, y.CPUPct) })
		prevS, prevP, prevT = s, p, now

		var flags []string
		if s.PressureLevel >= 4 {
			flags = append(flags, "memory critical")
		}
		if rt.Swapins+rt.Swapouts >= 50 {
			flags = append(flags, "swapping")
		}
		if rt.CPUBusy >= 90 {
			flags = append(flags, "CPU saturated")
		}
		if s.GPU >= 90 {
			flags = append(flags, "GPU saturated")
		}
		if s.Thermal >= 2 {
			flags = append(flags, "throttling")
		}
		if lat.P99Ms >= 10 || lat.MaxMs >= 100 {
			flags = append(flags, "LAG")
		}

		if lines%25 == 0 {
			fmt.Fprintln(w, header)
		}
		lines++
		gpu := "  -"
		if s.GPU >= 0 {
			gpu = fmt.Sprintf("%3d%%", s.GPU)
		}
		row := fmt.Sprintf("%-8s %4.0f%% %5.1f %-5s %7s %11s %7s %7s %11s %4s %12s  %s",
			now.Format("15:04:05"), rt.CPUBusy, s.Load[0], shortPressure(s.PressureLevel), hbShort(s.SwapUsed),
			kfmt(rt.Swapins)+"/"+kfmt(rt.Swapouts), kfmt(rt.Decompressions), kfmt(rt.Pageins),
			fmt.Sprintf("%.0f/%.0f", rt.DiskReadBps/(1<<20), rt.DiskWriteBps/(1<<20)), gpu,
			fmt.Sprintf("%.1f/%.0fms", lat.P99Ms, lat.MaxMs), fmt.Sprintf("%s %.0f%%", trunc(topApp.Name, 20), topApp.CPUPct))
		if len(flags) > 0 {
			alerts++
			row = st.yellow(row + "  ← " + strings.Join(flags, ", "))
		}
		fmt.Fprintln(w, row)

		if lat.MaxMs > worstLag.lagMax {
			worstLag = worst{now, lat.MaxMs, fmt.Sprintf("CPU %.0f%%, swap-ins %s/s, top app %s", rt.CPUBusy, kfmt(rt.Swapins), topApp.Name)}
		}
	}
}

func shortPressure(level int) string {
	switch {
	case level >= 4:
		return "CRIT"
	case level >= 2:
		return "warn"
	case level == 1:
		return "ok"
	}
	return "?"
}

func hbShort(b uint64) string {
	return strings.ReplaceAll(hb(b), " ", "")
}
