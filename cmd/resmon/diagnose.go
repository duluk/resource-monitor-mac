package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Diagnosis is the interpreted half of the report: what the numbers mean,
// what's fine, and what to do, in plain language.
type Diagnosis struct {
	Bottleneck   string   // "" when nothing is constrained
	Severe       bool     // the main bottleneck has critical findings
	Contributing []string // other constrained areas
	Story        []string // paragraphs explaining what's going on
	LooksFine    []string
	Actions      []string // ranked, most worthwhile first
}

var bottleneckAreas = map[string]string{
	"Memory": "Memory (RAM)", "CPU": "CPU", "Disk": "Disk", "GPU": "GPU", "Thermal": "Heat / power management",
}

func diagnose(r *Report) {
	d := &r.Diagnosis
	worst := map[string]Severity{}
	score := map[string]int{}
	for _, f := range r.Findings {
		if f.Severity >= Warning {
			worst[f.Area] = max(worst[f.Area], f.Severity)
		}
		if _, ok := bottleneckAreas[f.Area]; ok {
			score[f.Area] += [...]int{0, 3, 10}[f.Severity]
		}
	}
	var areas []string
	for a, s := range score {
		if s >= 3 {
			areas = append(areas, a)
		}
	}
	slices.SortFunc(areas, func(a, b string) int { return cmp.Or(cmp.Compare(score[b], score[a]), cmp.Compare(a, b)) })
	if len(areas) > 0 {
		d.Bottleneck, d.Severe = bottleneckAreas[areas[0]], score[areas[0]] >= 10
		for _, a := range areas[1:] {
			d.Contributing = append(d.Contributing, bottleneckAreas[a])
		}
	}
	_, memBad := worst["Memory"]
	_, cpuBad := worst["CPU"]
	_, lagBad := worst["Responsiveness"]

	ram := float64(r.Info.MemBytes)
	m := r.Mem
	pageMB := float64(m.PageSize) / (1 << 20)
	say := func(parts ...string) { d.Story = append(d.Story, strings.Join(parts, " ")) }

	if memBad {
		var p []string
		if len(areas) > 0 && areas[0] == "Memory" {
			p = append(p, "Memory is the main problem.")
		} else {
			p = append(p, "Memory is under strain.")
		}
		if m.Demand > r.Info.MemBytes {
			p = append(p, fmt.Sprintf("The open apps want at least %s of memory, but this Mac has %s.", hb(m.Demand), hb(r.Info.MemBytes)))
		}
		p = append(p, fmt.Sprintf("macOS is making up the difference by compressing %s of data into %s of RAM and keeping %s in swap on the SSD.",
			hb(m.CompressedOriginal), hb(m.Compressed), hb(m.SwapUsed)))
		if rate := r.Avg.Swapins + r.Avg.Swapouts; rate >= 10 {
			p = append(p, fmt.Sprintf("While measuring, it moved %.0f MB/s between RAM and the SSD (peak %s swap-ins/s).", rate*pageMB, kfmt(r.Peak.Swapins)))
		}
		p = append(p, "That is the most likely cause of laggy typing and slow app switching: when an app needs memory that was compressed or swapped out, it stops until macOS brings it back.")
		say(p...)

		var big []string
		for _, a := range r.Apps {
			share := float64(a.Mem) / ram
			if share < 0.1 || len(big) == 4 {
				break
			}
			if len(big) == 0 {
				big = append(big, fmt.Sprintf("%s (%s, %.0f%% of RAM, across %d processes)", a.Name, hb(a.Mem), share*100, a.Procs))
			} else {
				big = append(big, fmt.Sprintf("%s (%s)", a.Name, hb(a.Mem)))
			}
		}
		if len(big) > 0 {
			say("Most of that memory belongs to", joinAnd(big)+".",
				"App totals can add up to more than the installed RAM because they include their compressed and swapped-out memory.")
		}
	}

	if r.Info.DiskTotal > 0 && worst["Disk"] >= Warning && float64(r.Info.DiskFree)/float64(r.Info.DiskTotal) < 0.15 {
		say(fmt.Sprintf("The startup disk is %.0f%% full (%s free).", 100-100*float64(r.Info.DiskFree)/float64(r.Info.DiskTotal), hb(r.Info.DiskFree)),
			"Swap files live there, so there's little room for swap to grow; when it runs out, macOS stalls or starts asking you to quit apps.")
	}

	if cpuBad {
		byCPU := slices.SortedFunc(slices.Values(r.Apps), func(x, y AppUsage) int { return cmp.Compare(y.CPUPct, x.CPUPct) })
		s := fmt.Sprintf("The CPU is busy too: %.0f%% on average (peak %.0f%%)", r.Avg.CPUBusy, r.Peak.CPUBusy)
		if len(byCPU) > 0 {
			s += fmt.Sprintf(", with %s the biggest user at %.0f%% of a core", byCPU[0].Name, byCPU[0].CPUPct)
		}
		say(s + ".")
	}
	if worst["Thermal"] >= Warning {
		say("The Mac is also being slowed down deliberately, by heat or Low Power Mode (see findings).")
	}
	if worst["GPU"] >= Warning {
		say(fmt.Sprintf("The GPU is %d%% busy on average, which delays every screen update.", r.GPUAvg))
	}

	if l := r.Latency; l.Samples > 0 {
		switch {
		case lagBad:
			say(fmt.Sprintf("A test thread running at UI priority was woken up to %.0f ms late (p99 %.1f ms), so apps are also waiting for CPU time, not just memory.", l.MaxMs, l.P99Ms))
		case memBad:
			say(fmt.Sprintf("A test thread running at UI priority was never more than %.0f ms late, so the CPU isn't what's slowing input. The stalls come from memory being swapped back in, which this probe can't see.", max(1, l.MaxMs)))
		}
	}

	if d.Bottleneck == "" {
		say(fmt.Sprintf("Nothing in this %.0f-second sample looks constrained.", r.Seconds),
			"If the lag comes and goes, leave `resmon -watch` running and look at the lines from the moment it happens.")
		if lagBad {
			say("The UI-priority latency probe did see delays, though, so something intermittent is going on.")
		}
	}

	// What looks fine, so nobody chases it.
	if !memBad {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("Memory: pressure %s, %s in swap, %.0f swap-ins/s", pressureWord(m.PeakPressureLevel), hb(m.SwapUsed), r.Avg.Swapins))
	}
	if !cpuBad {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("CPU: %.0f%% busy on average (peak %.0f%%), load %.1f on %d cores", r.Avg.CPUBusy, r.Peak.CPUBusy, r.Load[0], r.Info.NCPU))
	}
	if !lagBad && r.Latency.Samples > 0 {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("Scheduling: a UI-priority thread was delayed at most %.0f ms (p99 %.1f ms)", max(1, r.Latency.MaxMs), r.Latency.P99Ms))
	}
	if worst["GPU"] < Warning && r.GPUAvg >= 0 {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("GPU: %d%% busy on average (peak %d%%)", r.GPUAvg, r.GPUPeak))
	}
	if worst["Disk"] < Warning {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("Disk: %s free, %.2f ms per I/O", hb(r.Info.DiskFree), r.Avg.DiskLatencyMs))
	} else if r.Avg.DiskLatencyMs < 15 {
		d.LooksFine = append(d.LooksFine, fmt.Sprintf("Disk speed: %.2f ms per I/O despite the load (the problem is free space, not the SSD)", r.Avg.DiskLatencyMs))
	}
	if worst["Thermal"] < Warning {
		d.LooksFine = append(d.LooksFine, "Heat/power: thermal state "+[...]string{"nominal", "fair", "serious", "critical"}[min(r.Thermal, 3)]+", Low Power Mode off")
	}
}

func pressureWord(level int) string {
	switch {
	case level >= 4:
		return "critical"
	case level >= 2:
		return "elevated"
	}
	return "normal"
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
