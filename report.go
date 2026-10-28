package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

type style struct{ on bool }

func (s style) wrap(code, txt string) string {
	if !s.on {
		return txt
	}
	return "\x1b[" + code + "m" + txt + "\x1b[0m"
}
func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) red(t string) string    { return s.wrap("1;31", t) }
func (s style) yellow(t string) string { return s.wrap("33", t) }
func (s style) green(t string) string  { return s.wrap("32", t) }
func (s style) cyan(t string) string   { return s.wrap("36", t) }

func (s style) sev(v Severity) string {
	switch v {
	case Critical:
		return s.red("✖ CRITICAL")
	case Warning:
		return s.yellow("▲ WARNING ")
	}
	return s.cyan("• INFO    ")
}

func printReport(w io.Writer, r *Report, top int, st style) {
	in := r.Info
	fmt.Fprintf(w, "%s — %s, %d cores (%dP+%dE), %s RAM, macOS %s (%s)\n",
		st.bold("Mac resource check"), in.CPU, in.NCPU, in.PCores, in.ECores, hb(in.MemBytes), in.OSVersion, in.Model)
	line := fmt.Sprintf("Sampled for %.0fs", r.Seconds)
	if !in.Boot.IsZero() {
		line += " · up " + humanDuration(time.Since(in.Boot))
	}
	fmt.Fprintln(w, line)
	if !in.Root && r.LimitedProcesses > 0 {
		fmt.Fprintln(w, st.dim(fmt.Sprintf("Not running as root: %d system processes report CPU and RSS only (no footprint/disk/page-ins). Run with sudo for full detail.", r.LimitedProcesses)))
	}
	fmt.Fprintln(w)

	m := r.Mem
	fmt.Fprintln(w, st.bold("MEMORY"))
	fmt.Fprintf(w, "  RAM %s: used %s (apps %s · wired %s · compressed %s holding %s) · cached files %s · free %s\n",
		hb(m.Total), hb(m.Used), hb(m.App), hb(m.Wired), hb(m.Compressed), hb(m.CompressedOriginal), hb(m.Cached), hb(m.Free))
	fmt.Fprintf(w, "  Swap %s of %s · pressure %s (≈%d%% available)\n", hb(m.SwapUsed), hb(m.SwapTotal), pressureName(m.PressureLevel, st), m.FreeLevel)
	fmt.Fprintf(w, "  Paging per second (avg / peak): swap-in %s / %s · swap-out %s / %s · page-in %s / %s · compress %s / %s · decompress %s / %s\n",
		kfmt(r.Avg.Swapins), kfmt(r.Peak.Swapins), kfmt(r.Avg.Swapouts), kfmt(r.Peak.Swapouts),
		kfmt(r.Avg.Pageins), kfmt(r.Peak.Pageins), kfmt(r.Avg.Compressions), kfmt(r.Peak.Compressions),
		kfmt(r.Avg.Decompressions), kfmt(r.Peak.Decompressions))
	fmt.Fprintln(w)

	fmt.Fprintf(w, "%s  avg %.0f%% busy (user %.0f%%, system %.0f%%) · peak %.0f%% · load %.1f %.1f %.1f on %d cores\n",
		st.bold("CPU    "), r.Avg.CPUBusy, r.Avg.CPUUser, r.Avg.CPUSys, r.Peak.CPUBusy, r.Load[0], r.Load[1], r.Load[2], in.NCPU)
	if r.GPUAvg >= 0 {
		fmt.Fprintf(w, "%s  avg %d%% · peak %d%%\n", st.bold("GPU    "), r.GPUAvg, r.GPUPeak)
	}
	fmt.Fprintf(w, "%s  read %s/s · write %s/s · %.0f ops/s · %.2f ms/op · %s free of %s\n",
		st.bold("DISK   "), hb(uint64(r.Avg.DiskReadBps)), hb(uint64(r.Avg.DiskWriteBps)), r.Avg.DiskOps, r.Avg.DiskLatencyMs, hb(in.DiskFree), hb(in.DiskTotal))
	lp := "off"
	if in.LowPowerMode {
		lp = st.yellow("ON")
	}
	fmt.Fprintf(w, "%s  %s · Low Power Mode %s\n", st.bold("THERMAL"), thermalName(r.Thermal, st), lp)
	if l := r.Latency; l.Samples > 0 {
		fmt.Fprintf(w, "%s  UI-priority wakeup lateness: p50 %.2f ms · p99 %.1f ms · max %.0f ms · %d > 16 ms · %d > 100 ms\n",
			st.bold("LATENCY"), l.P50Ms, l.P99Ms, l.MaxMs, l.Over16Ms, l.Over100Ms)
		fmt.Fprintln(w, st.dim("         (measures CPU scheduling delay only; stalls from swapped-out app memory don't show up here)"))
	}
	fmt.Fprintln(w)

	partial := false
	fmt.Fprintln(w, st.bold("TOP APPS BY MEMORY")+st.dim("  (helper/child processes grouped under the app responsible for them)"))
	fmt.Fprintf(w, "  %-28s %5s %9s %7s %9s %9s  %s\n", "App", "Procs", "Memory", "CPU%", "Disk I/O", "Page-ins", "Largest process")
	for _, a := range r.Apps[:min(top, len(r.Apps))] {
		mark := " "
		if a.Partial {
			mark, partial = "*", true
		}
		fmt.Fprintf(w, "  %-28s %5d %8s%s %7.1f %9s %9s  %s (%s)\n",
			trunc(a.Name, 28), a.Procs, hb(a.Mem), mark, a.CPUPct, hb(a.DiskR+a.DiskW), hb(a.Pageins*m.PageSize), trunc(a.Largest, 30), hb(a.LargestMem))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, st.bold("TOP APPS BY CPU"))
	fmt.Fprintf(w, "  %-28s %5s %7s %9s\n", "App", "Procs", "CPU%", "Memory")
	byCPU := slices.SortedFunc(slices.Values(r.Apps), func(x, y AppUsage) int { return cmp.Compare(y.CPUPct, x.CPUPct) })
	for _, a := range byCPU[:min(top, len(byCPU))] {
		fmt.Fprintf(w, "  %-28s %5d %7.1f %9s\n", trunc(a.Name, 28), a.Procs, a.CPUPct, hb(a.Mem))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, st.bold("TOP PROCESSES BY CPU"))
	fmt.Fprintf(w, "  %6s  %-32s %-24s %7s %9s\n", "PID", "Process", "App", "CPU%", "Memory")
	for _, p := range r.Procs[:min(top, len(r.Procs))] {
		mark := " "
		if !p.Full {
			mark, partial = "*", true
		}
		fmt.Fprintf(w, "  %6d  %-32s %-24s %7.1f %8s%s\n", p.PID, trunc(p.Name, 32), trunc(p.App, 24), p.CPUPct, hb(p.Mem), mark)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.dim("CPU% is percent of one core (400% = four cores busy). Memory is physical footprint, including compressed and swapped parts."))
	if partial {
		fmt.Fprintln(w, st.dim("* resident memory (RSS) from ps; detailed stats need sudo."))
	}
	fmt.Fprintln(w)
	printDiagnosis(w, r, st)
}

func printDiagnosis(w io.Writer, r *Report, st style) {
	d := r.Diagnosis
	fmt.Fprintln(w, st.bold(strings.Repeat("━", 40)+" DIAGNOSIS "+strings.Repeat("━", 40)))
	fmt.Fprint(w, st.bold("VERDICT  "))
	switch {
	case d.Bottleneck == "":
		fmt.Fprintln(w, st.green("No resource bottleneck detected during this sample."))
	case d.Severe:
		fmt.Fprintln(w, st.red("Main bottleneck: "+d.Bottleneck))
	default:
		fmt.Fprintln(w, st.yellow("Likely bottleneck: "+d.Bottleneck))
	}
	if len(d.Contributing) > 0 {
		fmt.Fprintln(w, "         Also contributing: "+strings.Join(d.Contributing, ", "))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, st.bold("What's going on"))
	for i, p := range d.Story {
		if i > 0 {
			fmt.Fprintln(w)
		}
		for _, l := range wrapText(p, 96) {
			fmt.Fprintln(w, "  "+l)
		}
	}
	fmt.Fprintln(w)

	if len(r.Findings) > 0 {
		fmt.Fprintln(w, st.bold("Findings")+st.dim("  (most serious first)"))
		for _, f := range r.Findings {
			fmt.Fprintf(w, "  %s %s %s\n", st.sev(f.Severity), st.dim("["+f.Area+"]"), st.bold(f.Title))
			for _, l := range wrapText(f.Detail, 84) {
				fmt.Fprintf(w, "               %s\n", st.dim(l))
			}
		}
		fmt.Fprintln(w)
	}

	if len(d.LooksFine) > 0 {
		fmt.Fprintln(w, st.bold("Looks fine")+st.dim("  (not worth chasing)"))
		for _, l := range d.LooksFine {
			fmt.Fprintf(w, "  %s %s\n", st.green("✓"), l)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, st.bold("What to do, in order"))
	if len(d.Actions) == 0 {
		fmt.Fprintln(w, "  Nothing based on this sample. If the lag is intermittent, run `resmon -watch` while you work.")
	}
	for i, a := range d.Actions {
		num := fmt.Sprintf("%d.", i+1)
		for j, l := range wrapText(a, 92) {
			if j == 0 {
				fmt.Fprintf(w, "  %s %s\n", st.bold(fmt.Sprintf("%-3s", num)), l)
			} else {
				fmt.Fprintf(w, "      %s\n", l)
			}
		}
	}
}

func pressureName(level int, st style) string {
	switch {
	case level >= 4:
		return st.red("CRITICAL")
	case level >= 2:
		return st.yellow("WARN")
	case level == 1:
		return st.green("normal")
	}
	return "unknown"
}

func thermalName(t int, st style) string {
	switch t {
	case 0:
		return st.green("nominal")
	case 1:
		return "fair"
	case 2:
		return st.yellow("serious (throttling)")
	case 3:
		return st.red("critical (throttling)")
	}
	return "unknown"
}

func hb(b uint64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.1f GB", f/(1<<30))
	case f >= 10<<20:
		return fmt.Sprintf("%.0f MB", f/(1<<20))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KB", f/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func kfmt(v float64) string {
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.0fk", v/1000)
	case v >= 1000:
		return fmt.Sprintf("%.1fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
}

func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func wrapText(s string, width int) []string {
	var lines []string
	var cur strings.Builder
	for _, word := range strings.Fields(s) {
		if cur.Len() > 0 && cur.Len()+1+len(word) > width {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}
