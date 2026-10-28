package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Severity int

const (
	Info Severity = iota
	Warning
	Critical
)

func (s Severity) String() string               { return [...]string{"INFO", "WARNING", "CRITICAL"}[s] }
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

type Finding struct {
	Severity Severity
	Area     string // Memory, CPU, Disk, GPU, Thermal, Responsiveness, System
	Title    string
	Detail   string
}

// Rates are per-second values between two SysSamples. CPU percentages are of
// total capacity (all cores = 100%); paging figures are in pages/s.
type Rates struct {
	CPUBusy, CPUUser, CPUSys                                           float64
	Pageins, Pageouts, Swapins, Swapouts, Compressions, Decompressions float64
	DiskReadBps, DiskWriteBps, DiskOps, DiskLatencyMs                  float64
}

type MemSummary struct {
	Total, Used, App, Wired, Compressed, CompressedOriginal, Cached, Free uint64
	SwapUsed, SwapTotal                                                   uint64
	PageSize                                                              uint64
	PressureLevel, PeakPressureLevel, FreeLevel                           int
	// Demand is a lower bound on how much memory the current workload wants
	// resident: app + wired + uncompressed size of compressed pages + swap.
	Demand uint64
}

type ProcUsage struct {
	PID                   int
	Name, App             string
	CPUPct                float64 // % of one core
	Mem                   uint64  // phys footprint (RSS when !Full)
	DiskR, DiskW, Pageins uint64  // during the sample window (Pageins in pages)
	Full                  bool
}

type AppUsage struct {
	Name                  string
	Procs                 int
	CPUPct                float64
	Mem                   uint64
	DiskR, DiskW, Pageins uint64
	Largest               string
	LargestMem            uint64
	Partial               bool // some member processes only had ps-level data
}

type Report struct {
	Info             SystemInfo
	Seconds          float64
	Avg, Peak        Rates
	Mem              MemSummary
	GPUAvg, GPUPeak  int // -1 when unavailable
	Thermal          int
	Load             [3]float64
	Latency          LatencyStats
	Apps             []AppUsage  // by memory
	Procs            []ProcUsage // by CPU
	LimitedProcesses int
	Findings         []Finding
	Diagnosis        Diagnosis
	AIAnalysis       string `json:",omitempty"`
}

func delta(a, b uint64) float64 {
	if b < a {
		return 0
	}
	return float64(b - a)
}

func ratesBetween(a, b SysSample) Rates {
	secs := b.At.Sub(a.At).Seconds()
	if secs <= 0 {
		return Rates{}
	}
	var r Rates
	u, s, n, i := delta(a.CPU.User, b.CPU.User), delta(a.CPU.Sys, b.CPU.Sys), delta(a.CPU.Nice, b.CPU.Nice), delta(a.CPU.Idle, b.CPU.Idle)
	if tot := u + s + n + i; tot > 0 {
		r.CPUBusy = 100 * (u + s + n) / tot
		r.CPUUser = 100 * (u + n) / tot
		r.CPUSys = 100 * s / tot
	}
	r.Pageins = delta(a.VM.Pageins, b.VM.Pageins) / secs
	r.Pageouts = delta(a.VM.Pageouts, b.VM.Pageouts) / secs
	r.Swapins = delta(a.VM.Swapins, b.VM.Swapins) / secs
	r.Swapouts = delta(a.VM.Swapouts, b.VM.Swapouts) / secs
	r.Compressions = delta(a.VM.Compressions, b.VM.Compressions) / secs
	r.Decompressions = delta(a.VM.Decompressions, b.VM.Decompressions) / secs
	if a.Disk.OK && b.Disk.OK {
		r.DiskReadBps = delta(a.Disk.ReadBytes, b.Disk.ReadBytes) / secs
		r.DiskWriteBps = delta(a.Disk.WriteBytes, b.Disk.WriteBytes) / secs
		ops := delta(a.Disk.ReadOps, b.Disk.ReadOps) + delta(a.Disk.WriteOps, b.Disk.WriteOps)
		r.DiskOps = ops / secs
		if ops > 0 {
			r.DiskLatencyMs = (delta(a.Disk.ReadNs, b.Disk.ReadNs) + delta(a.Disk.WriteNs, b.Disk.WriteNs)) / ops / 1e6
		}
	}
	return r
}

func maxRates(a, b Rates) Rates {
	return Rates{
		CPUBusy: max(a.CPUBusy, b.CPUBusy), CPUUser: max(a.CPUUser, b.CPUUser), CPUSys: max(a.CPUSys, b.CPUSys),
		Pageins: max(a.Pageins, b.Pageins), Pageouts: max(a.Pageouts, b.Pageouts),
		Swapins: max(a.Swapins, b.Swapins), Swapouts: max(a.Swapouts, b.Swapouts),
		Compressions: max(a.Compressions, b.Compressions), Decompressions: max(a.Decompressions, b.Decompressions),
		DiskReadBps: max(a.DiskReadBps, b.DiskReadBps), DiskWriteBps: max(a.DiskWriteBps, b.DiskWriteBps),
		DiskOps: max(a.DiskOps, b.DiskOps), DiskLatencyMs: max(a.DiskLatencyMs, b.DiskLatencyMs),
	}
}

func memSummary(info SystemInfo, s SysSample) MemSummary {
	ps := s.VM.PageSize
	m := MemSummary{
		Total:              info.MemBytes,
		PageSize:           ps,
		Wired:              s.VM.Wired * ps,
		Compressed:         s.VM.Compressor * ps,
		CompressedOriginal: s.VM.UncompressedInCompressor * ps,
		Cached:             (s.VM.External + s.VM.Purgeable) * ps,
		Free:               s.VM.Free * ps,
		SwapUsed:           s.SwapUsed,
		SwapTotal:          s.SwapTotal,
		PressureLevel:      s.PressureLevel,
		FreeLevel:          s.FreeLevel,
	}
	if s.VM.Internal > s.VM.Purgeable {
		m.App = (s.VM.Internal - s.VM.Purgeable) * ps
	}
	m.Used = m.App + m.Wired + m.Compressed
	m.Demand = m.App + m.Wired + m.CompressedOriginal + m.SwapUsed
	return m
}

// bundleName returns the outermost .app bundle in path, so helper processes
// nested inside an app (Electron/Chromium/Firefox helpers) group with it.
func bundleName(path string) (string, bool) {
	i := strings.Index(path, ".app/")
	if i < 0 {
		return "", false
	}
	return filepath.Base(path[:i]), true
}

func procName(p *Proc) string {
	switch {
	case p.Path != "":
		return filepath.Base(p.Path)
	case p.Name != "":
		return p.Name
	case p.PID == 0:
		return "kernel_task"
	}
	return fmt.Sprintf("pid %d", p.PID)
}

// appName attributes a process to the app responsible for it (e.g. Safari for
// WebKit content processes, a terminal for commands run in it), falling back
// to its own bundle or executable name.
func appName(p *Proc, procs map[int]*Proc) string {
	if p.RespPID > 1 && p.RespPID != p.PID {
		if r, ok := procs[p.RespPID]; ok {
			if n, ok := bundleName(r.Path); ok {
				return n
			}
		}
	}
	if n, ok := bundleName(p.Path); ok {
		return n
	}
	return procName(p)
}

func procUsages(p0, p1 map[int]*Proc, secs float64) []ProcUsage {
	var out []ProcUsage
	for pid, b := range p1 {
		a := p0[pid]
		if a == nil || a.Path != b.Path {
			a = &Proc{} // started during the window (or pid reused): count everything it did
		}
		out = append(out, ProcUsage{
			PID:     pid,
			Name:    procName(b),
			App:     appName(b, p1),
			CPUPct:  delta(a.CPUNs, b.CPUNs) / secs / 1e7,
			Mem:     b.Footprint,
			DiskR:   uint64(delta(a.DiskR, b.DiskR)),
			DiskW:   uint64(delta(a.DiskW, b.DiskW)),
			Pageins: uint64(delta(a.Pageins, b.Pageins)),
			Full:    b.Full,
		})
	}
	slices.SortFunc(out, func(x, y ProcUsage) int { return cmp.Compare(y.CPUPct, x.CPUPct) })
	return out
}

func aggregateApps(procs []ProcUsage) []AppUsage {
	m := map[string]*AppUsage{}
	for _, p := range procs {
		a := m[p.App]
		if a == nil {
			a = &AppUsage{Name: p.App}
			m[p.App] = a
		}
		a.Procs++
		a.CPUPct += p.CPUPct
		a.Mem += p.Mem
		a.DiskR += p.DiskR
		a.DiskW += p.DiskW
		a.Pageins += p.Pageins
		a.Partial = a.Partial || !p.Full
		if p.Mem > a.LargestMem {
			a.Largest, a.LargestMem = p.Name, p.Mem
		}
	}
	out := make([]AppUsage, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(x, y AppUsage) int { return cmp.Compare(y.Mem, x.Mem) })
	return out
}

// procSecs is the time between the two process snapshots p0 and p1.
func buildReport(info SystemInfo, ticks []SysSample, p0, p1 map[int]*Proc, procSecs float64, lat []float64) *Report {
	first, last := ticks[0], ticks[len(ticks)-1]
	r := &Report{
		Info:    info,
		Seconds: last.At.Sub(first.At).Seconds(),
		Avg:     ratesBetween(first, last),
		Mem:     memSummary(info, last),
		Load:    last.Load,
		Latency: latencyStats(lat),
		GPUAvg:  -1,
		GPUPeak: -1,
	}
	gpuSum, gpuN := 0, 0
	for i, t := range ticks {
		if i > 0 {
			r.Peak = maxRates(r.Peak, ratesBetween(ticks[i-1], t))
		}
		r.Mem.PeakPressureLevel = max(r.Mem.PeakPressureLevel, t.PressureLevel)
		r.Thermal = max(r.Thermal, t.Thermal)
		if t.GPU >= 0 {
			gpuSum += t.GPU
			gpuN++
			r.GPUPeak = max(r.GPUPeak, t.GPU)
		}
	}
	if gpuN > 0 {
		r.GPUAvg = gpuSum / gpuN
	}
	r.Procs = procUsages(p0, p1, procSecs)
	r.Apps = aggregateApps(r.Procs)
	for _, p := range p1 {
		if !p.Full {
			r.LimitedProcesses++
		}
	}
	r.Findings, r.Diagnosis.Actions = analyze(r)
	diagnose(r)
	return r
}

const gb = 1 << 30

// Action priorities: lower comes first. Cheap, high-impact fixes lead; buying hardware is last.
const (
	prioQuickFix  = 10
	prioMainHog   = 20
	prioDisk      = 30
	prioOtherHogs = 40
	prioCPU       = 50
	prioRestart   = 60
	prioTune      = 70
	prioHardware  = 90
)

// analyze turns the measurements into findings (what's wrong) and a ranked,
// de-duplicated list of actions (what to do about it).
func analyze(r *Report) ([]Finding, []string) {
	var f []Finding
	add := func(sev Severity, area, title, detail string) {
		f = append(f, Finding{sev, area, title, detail})
	}
	type action struct {
		prio int
		text string
	}
	var acts []action
	act := func(prio int, format string, args ...any) {
		acts = append(acts, action{prio, fmt.Sprintf(format, args...)})
	}
	ram := float64(r.Info.MemBytes)
	m := r.Mem
	pageMB := float64(m.PageSize) / (1 << 20)
	uptime := time.Duration(0)
	if !r.Info.Boot.IsZero() {
		uptime = time.Since(r.Info.Boot)
	}

	// ---- Memory ----
	switch worst := max(m.PressureLevel, m.PeakPressureLevel); {
	case worst >= 4:
		add(Critical, "Memory", "Memory pressure is CRITICAL (red in Activity Monitor)",
			"The kernel has run out of comfortable headroom and is aggressively compressing and swapping. Apps stall whenever they touch memory that was compressed or swapped out, which feels exactly like delayed typing and sluggish app switching.")
	case worst >= 2:
		add(Warning, "Memory", "Memory pressure is elevated (yellow in Activity Monitor)",
			fmt.Sprintf("macOS considers memory scarce (only ~%d%% available). It's compressing memory and swapping to SSD to cope, which costs CPU and adds stalls when apps touch that memory again.", m.FreeLevel))
	}

	swapRatio := float64(m.SwapUsed) / ram
	switch {
	case swapRatio >= 1:
		add(Critical, "Memory", fmt.Sprintf("Swap in use: %s — %.1f× your %s of RAM", hb(m.SwapUsed), swapRatio, hb(r.Info.MemBytes)),
			"Swap is memory that didn't fit in RAM and was written to the SSD. Some swap is normal; more swap than RAM means the open apps want far more memory than the Mac has. Switching to an app whose memory was swapped out forces macOS to read it back from disk before the app can respond.")
	case swapRatio >= 0.25:
		add(Warning, "Memory", fmt.Sprintf("Swap in use: %s (%.0f%% of RAM)", hb(m.SwapUsed), swapRatio*100),
			"A significant amount of memory has been pushed out to the SSD. Apps whose memory lives there respond slowly the first time you return to them.")
	case m.SwapUsed >= gb:
		add(Info, "Memory", fmt.Sprintf("Swap in use: %s", hb(m.SwapUsed)),
			"Some memory has been swapped out at some point. Harmless unless it's actively being swapped back in (see paging rates).")
	}
	if swapRatio >= 0.5 && uptime >= 48*time.Hour {
		act(prioRestart, "Restart the Mac. It has been up %s and has %s in swap; a restart clears swap and any memory leaked by long-running apps and system services.",
			humanDuration(uptime), hb(m.SwapUsed))
	}

	demandRatio := float64(m.Demand) / ram
	if demandRatio >= 1.1 {
		sev := Warning
		if demandRatio >= 1.5 {
			sev = Critical
		}
		add(sev, "Memory", fmt.Sprintf("Workload wants ≈%s of memory; this Mac has %s", hb(m.Demand), hb(r.Info.MemBytes)),
			fmt.Sprintf("Estimate = app memory (%s) + wired (%s) + compressed data at its original size (%s) + swap (%s). It's a lower bound, since swapped data is counted at its compressed size.",
				hb(m.App), hb(m.Wired), hb(m.CompressedOriginal), hb(m.SwapUsed)))
		act(prioHardware, "Longer term: running this set of apps without heavy swapping needs roughly %d GB of RAM. With %s, the alternative is keeping fewer heavy apps open at once.",
			suggestRAM(m.Demand), hb(r.Info.MemBytes))
	}

	swapRate := r.Avg.Swapins + r.Avg.Swapouts
	switch {
	case swapRate >= 200:
		add(Critical, "Memory", fmt.Sprintf("Actively swapping: %.0f pages/s (%.1f MB/s) to/from the SSD", swapRate, swapRate*pageMB),
			"Memory is being shuffled between RAM and disk right now, not just parked there. Any app touching swapped memory blocks until the read completes. This is thrashing: the working set of the open apps doesn't fit in RAM.")
	case swapRate >= 10 || r.Peak.Swapins >= 300:
		add(Warning, "Memory", fmt.Sprintf("Swap traffic during sample: avg %.0f pages/s, peak %.0f swap-ins/s", swapRate, r.Peak.Swapins),
			"Some memory was being read back from (or written to) swap while measuring. Each swap-in is a stall for whichever app needed that memory.")
	}

	if churn := r.Avg.Compressions + r.Avg.Decompressions; churn >= 1000 {
		sev := Info
		if churn >= 5000 {
			sev = Warning
		}
		add(sev, "Memory", fmt.Sprintf("Memory compressor busy: %.0f compressions + %.0f decompressions per second", r.Avg.Compressions, r.Avg.Decompressions),
			fmt.Sprintf("%s of compressed data currently sits in %s of RAM. Compression is faster than swap, but every access to compressed memory costs CPU time and a small stall.", hb(m.CompressedOriginal), hb(m.Compressed)))
	}

	if r.Avg.Pageins >= 300 && m.PeakPressureLevel >= 2 {
		add(Warning, "Memory", fmt.Sprintf("Re-reading %.1f MB/s of files and app code from disk (%.0f page-ins/s)", r.Avg.Pageins*pageMB, r.Avg.Pageins),
			"Under memory pressure macOS drops cached file data and app code, which then has to be read back in the next time it's needed.")
	}

	// ri_pageins only counts file-backed page-ins (code, mapped files), not swap-ins,
	// so this shows who is re-reading evicted files rather than who is swapping.
	var rereads []string
	for _, a := range slices.SortedFunc(slices.Values(r.Apps), func(x, y AppUsage) int { return cmp.Compare(y.Pageins, x.Pageins) }) {
		if a.Pageins*m.PageSize < 5<<20 || len(rereads) == 5 {
			break
		}
		rereads = append(rereads, fmt.Sprintf("%s (%s)", a.Name, hb(a.Pageins*m.PageSize)))
	}
	if len(rereads) > 0 {
		add(Info, "Memory", "Apps re-reading evicted code/files from disk during the sample",
			"Each of these page-ins was a short pause in that app: "+strings.Join(rereads, ", ")+".")
	}

	memStrained := m.PeakPressureLevel >= 2 || demandRatio >= 1.1 || swapRate >= 10
	var others []string
	for i, a := range r.Apps {
		share := float64(a.Mem) / ram
		if share >= 0.25 {
			sev := Info
			switch {
			case m.PeakPressureLevel >= 2 && share >= 0.5:
				sev = Critical
			case m.PeakPressureLevel >= 2:
				sev = Warning
			}
			add(sev, "Memory", fmt.Sprintf("%s is using %s (%.0f%% of RAM) across %d process(es)", a.Name, hb(a.Mem), share*100, a.Procs),
				fmt.Sprintf("Its largest process is %s at %s. Memory here includes parts that are compressed or swapped out.", a.Largest, hb(a.LargestMem)))
		}
		if !memStrained {
			continue
		}
		switch {
		case i == 0 && share >= 0.25:
			act(prioMainHog, "Cut down %s first: it holds %s (%.0f%% of RAM) across %d processes, the largest being %s (%s). %s",
				a.Name, hb(a.Mem), share*100, a.Procs, a.Largest, hb(a.LargestMem), memoryHint(a))
		case len(others) < 4 && share >= 0.05:
			others = append(others, fmt.Sprintf("%s (%s)", a.Name, hb(a.Mem)))
		}
	}
	if len(others) > 0 {
		act(prioOtherHogs, "Quit apps you aren't actively using (closing their windows isn't enough). The next biggest are %s.", strings.Join(others, ", "))
	}

	// ---- CPU ----
	switch busy := r.Avg.CPUBusy; {
	case busy >= 90:
		add(Critical, "CPU", fmt.Sprintf("CPU saturated: %.0f%% busy on average across %d cores", busy, r.Info.NCPU),
			"Every core is busy, so interactive threads have to wait their turn.")
	case busy >= 70:
		add(Warning, "CPU", fmt.Sprintf("CPU heavily loaded: %.0f%% busy on average", busy),
			"There's little spare CPU for bursts like redrawing a window or handling input.")
	case r.Peak.CPUBusy >= 95:
		add(Info, "CPU", fmt.Sprintf("CPU briefly hit %.0f%% (average %.0f%%)", r.Peak.CPUBusy, busy),
			"Short spikes to full load can cause momentary hitches.")
	}
	if n := float64(r.Info.NCPU); n > 0 && r.Load[0] >= 1.5*n {
		add(Warning, "CPU", fmt.Sprintf("Long run queue: 1-minute load average %.1f on %d cores", r.Load[0], r.Info.NCPU),
			"More threads want to run than there are cores, so some are waiting.")
	}
	if r.Avg.CPUSys >= 30 {
		add(Warning, "CPU", fmt.Sprintf("%.0f%% of CPU time is spent in the kernel", r.Avg.CPUSys),
			"High system time usually means heavy paging/compression, lots of I/O, or a security agent intercepting file access.")
	}

	for _, a := range slices.SortedFunc(slices.Values(r.Apps), func(x, y AppUsage) int { return cmp.Compare(y.CPUPct, x.CPUPct) }) {
		if a.CPUPct < 100 {
			break
		}
		sev := Info
		if r.Avg.CPUBusy >= 50 || a.CPUPct >= 200 {
			sev = Warning
			act(prioCPU, "Find out why %s is keeping ≈%.1f cores busy. In a browser, its task manager shows which tabs are working; otherwise check what the app is doing, or restart it.",
				a.Name, a.CPUPct/100)
		}
		add(sev, "CPU", fmt.Sprintf("%s is using %.0f%% CPU (≈%.1f cores) across %d process(es)", a.Name, a.CPUPct, a.CPUPct/100, a.Procs),
			"Sustained multi-core usage by one app competes with everything else for CPU.")
	}

	for _, p := range r.Procs {
		if p.CPUPct < 90 {
			break
		}
		if p.Name == "WindowServer" || p.Name == "kernel_task" {
			continue
		}
		add(Info, "CPU", fmt.Sprintf("%s (pid %d, in %s) kept a core ~%.0f%% busy the whole time", p.Name, p.PID, p.App, p.CPUPct),
			"A process pinned near 100% of a core for the entire sample is often stuck in a loop or doing heavy background work.")
		act(prioCPU, "Check %s (pid %d, part of %s): it was pegged at ~%.0f%% of a core. If you don't expect it to be working, quit or restart it.", p.Name, p.PID, p.App, p.CPUPct)
	}

	for _, p := range r.Procs {
		switch p.Name {
		case "WindowServer":
			if p.CPUPct >= 20 {
				sev := Info
				if p.CPUPct >= 40 {
					sev = Warning
					act(prioTune, "Lighten WindowServer's load (%.0f%% CPU): close windows you don't need, especially ones with video or animation; turn on Reduce Transparency and Reduce Motion (System Settings → Accessibility → Display); use fewer or lower-resolution external displays.", p.CPUPct)
				}
				add(sev, "CPU", fmt.Sprintf("WindowServer is using %.0f%% CPU", p.CPUPct),
					"WindowServer composites everything on screen. When it's busy, drawing and input in every app lag behind.")
			}
			if p.Mem >= 2*gb {
				add(Info, "Memory", fmt.Sprintf("WindowServer is holding %s of memory", hb(p.Mem)),
					"This grows with the number of windows/displays and sometimes leaks over long uptimes. Logging out and back in resets it.")
			}
		case "kernel_task":
			if p.CPUPct >= 50 {
				add(Warning, "Thermal", fmt.Sprintf("kernel_task is using %.0f%% CPU", p.CPUPct),
					"kernel_task deliberately occupies CPU to cool the machine when it's too hot, and also does work on behalf of heavy I/O and swapping.")
				act(prioCPU, "Check cooling: kernel_task at %.0f%% CPU usually means the Mac is too hot. Give it airflow and reduce sustained load.", p.CPUPct)
			}
		}
	}

	daemonCPU := map[string]float64{}
	daemonIO := map[string]uint64{}
	for _, p := range r.Procs {
		if d, ok := daemonByName[p.Name]; ok {
			daemonCPU[d.label] += p.CPUPct
			daemonIO[d.label] += p.DiskR + p.DiskW
		}
	}
	for _, d := range knownDaemons {
		cpu, io := daemonCPU[d.label], float64(daemonIO[d.label])/r.Seconds
		if cpu < 25 && io < 10<<20 {
			continue
		}
		sev := Info
		if cpu >= 60 {
			sev = Warning
		}
		add(sev, "CPU", fmt.Sprintf("Background activity: %s (%.0f%% CPU, %s/s disk)", d.label, cpu, hb(uint64(io))), d.detail)
		if d.advice != "" {
			act(prioTune+5, "%s: %s", d.label, d.advice)
		}
	}

	// ---- Thermal / power ----
	switch r.Thermal {
	case 3:
		add(Critical, "Thermal", "Thermal state is CRITICAL", "The Mac is heavily throttling performance to cool down.")
		act(prioCPU, "Cool the Mac down: improve airflow, remove heavy load, and look for a stuck process.")
	case 2:
		add(Warning, "Thermal", "Thermal state is SERIOUS — performance is being throttled", "macOS is reducing CPU/GPU speed to manage heat.")
		act(prioCPU, "Cool the Mac down: improve airflow and reduce sustained load.")
	case 1:
		add(Info, "Thermal", "Thermal state is FAIR", "Slightly warm; minor throttling is possible under sustained load.")
	}
	if r.Info.LowPowerMode {
		add(Warning, "Thermal", "Low Power Mode is on", "Low Power Mode lowers CPU clock speeds and limits background work, which makes everything feel slower.")
		act(prioQuickFix, "Turn off Low Power Mode (System Settings → Battery) when you need performance.")
	}

	// ---- Disk ----
	if r.Info.DiskTotal > 0 {
		free, frac := r.Info.DiskFree, float64(r.Info.DiskFree)/float64(r.Info.DiskTotal)
		switch {
		case free < 10*gb || frac < 0.05:
			add(Critical, "Disk", fmt.Sprintf("Startup disk nearly full: %s free (%.0f%%)", hb(free), frac*100),
				fmt.Sprintf("Swap files live on this disk (%s right now). When macOS can't grow swap it stalls, shows \"out of application memory\", or kills apps; caches also get purged and rebuilt.", hb(m.SwapUsed)))
			act(prioDisk, "Free up disk space: only %s (%.0f%%) is free and swap needs room to grow. Aim for at least 15–20%% free; System Settings → General → Storage shows what's using it.", hb(free), frac*100)
		case free < 30*gb || frac < 0.15:
			add(Warning, "Disk", fmt.Sprintf("Startup disk getting full: %s free (%.0f%%)", hb(free), frac*100),
				fmt.Sprintf("With %s already in swap, low free space leaves little room for more.", hb(m.SwapUsed)))
			act(prioDisk+5, "Free up some disk space (%s free now) so swap and caches have room.", hb(free))
		}
	}
	if r.Avg.DiskOps >= 20 && r.Avg.DiskLatencyMs >= 15 {
		add(Warning, "Disk", fmt.Sprintf("Slow disk I/O: %.1f ms per operation on average (%.0f ops/s)", r.Avg.DiskLatencyMs, r.Avg.DiskOps),
			"Apple SSDs normally answer in well under a few ms; higher latency means the disk is queueing (often swap traffic or a backup/indexer).")
	}
	if tp := r.Avg.DiskReadBps + r.Avg.DiskWriteBps; tp >= 200<<20 {
		top := slices.MaxFunc(r.Apps, func(x, y AppUsage) int { return cmp.Compare(x.DiskR+x.DiskW, y.DiskR+y.DiskW) })
		add(Info, "Disk", fmt.Sprintf("Heavy disk throughput: %s/s average", hb(uint64(tp))),
			fmt.Sprintf("Largest contributor we could see: %s (%s during the sample).", top.Name, hb(top.DiskR+top.DiskW)))
	}

	// ---- GPU ----
	switch {
	case r.GPUAvg >= 85:
		add(Warning, "GPU", fmt.Sprintf("GPU busy: %d%% on average", r.GPUAvg),
			"The GPU draws every window. When it's saturated, screen updates (including echoing typed characters) are delayed.")
		act(prioCPU, "Reduce GPU load (%d%% busy): look for video, WebGL or animated content in background windows and tabs.", r.GPUAvg)
	case r.GPUPeak >= 98:
		add(Info, "GPU", fmt.Sprintf("GPU briefly hit %d%% (average %d%%)", r.GPUPeak, r.GPUAvg), "Short GPU spikes can cause dropped frames.")
	}

	// ---- Responsiveness ----
	if l := r.Latency; l.Samples > 0 {
		detail := fmt.Sprintf("A test thread at UI priority asked to wake up %d times; it woke late by p50 %.2f ms, p99 %.1f ms, worst %.0f ms (%d wakeups later than one 60 Hz frame).",
			l.Samples, l.P50Ms, l.P99Ms, l.MaxMs, l.Over16Ms)
		switch {
		case l.P99Ms >= 50 || l.Over100Ms >= 3:
			add(Critical, "Responsiveness", "Interactive threads are being delayed badly", detail)
		case l.P99Ms >= 10 || l.MaxMs >= 100:
			add(Warning, "Responsiveness", "Interactive threads are sometimes delayed", detail)
		case l.Over16Ms > 0:
			add(Info, "Responsiveness", "Occasional scheduling hiccups", detail)
		}
	}

	// ---- System ----
	if uptime >= 14*24*time.Hour {
		add(Info, "System", fmt.Sprintf("Up for %s since the last restart", humanDuration(uptime)),
			"Long uptimes let memory leaks and swap accumulate in long-running apps and system services.")
	}

	slices.SortStableFunc(f, func(a, b Finding) int { return cmp.Compare(b.Severity, a.Severity) })
	slices.SortStableFunc(acts, func(a, b action) int { return cmp.Compare(a.prio, b.prio) })
	var out []string
	for _, a := range acts {
		if !slices.Contains(out, a.text) {
			out = append(out, a.text)
		}
	}
	return f, out
}

// memoryHint suggests how to shrink an app's memory based on its shape: many
// helper processes usually means browser tabs or web views.
func memoryHint(a AppUsage) string {
	if a.Procs >= 5 {
		return "If it's a browser, close tabs or have it unload inactive ones; otherwise close windows/documents, or restart it to release memory it has accumulated."
	}
	return "Close documents/windows you don't need, or restart it to release memory it has accumulated."
}

func suggestRAM(demand uint64) int {
	want := float64(demand) * 1.2 / gb
	for _, s := range []int{16, 24, 32, 36, 48, 64, 96, 128, 192, 256} {
		if float64(s) >= want {
			return s
		}
	}
	return 256
}

type daemon struct {
	label, detail, advice string
	names                 []string
}

var knownDaemons = []daemon{
	{"Spotlight indexing",
		"Spotlight is (re)indexing files. Usually temporary after OS updates or large file changes.",
		"If it never settles, exclude big folders (code checkouts, VM images, caches) in System Settings → Spotlight → Search Privacy.",
		[]string{"mds", "mds_stores", "mdworker", "mdworker_shared", "mdsync", "corespotlightd"}},
	{"Time Machine backup", "A backup is running.", "If backups are constantly busy, exclude caches and large churn-y folders.",
		[]string{"backupd", "backupd-helper"}},
	{"Photos/media analysis", "Photos is analysing the library (faces, scenes, text).", "It's throttled when you're active; usually settles on its own.",
		[]string{"photoanalysisd", "mediaanalysisd", "photolibraryd"}},
	{"iCloud / file-provider sync", "Files are syncing with iCloud or another file provider (OneDrive, Dropbox, Google Drive).", "",
		[]string{"cloudd", "bird", "fileproviderd", "FileProvider", "OneDrive", "Dropbox", "Google Drive"}},
	{"macOS software update", "macOS is downloading or preparing a software update.", "Install the pending update so it stops working in the background.",
		[]string{"softwareupdated", "com.apple.MobileSoftwareUpdate.UpdateBrainService", "UpdateBrainService"}},
	{"Malware scanning (XProtect/Gatekeeper)", "Apple's built-in malware scanning is checking files or apps.", "",
		[]string{"XprotectService", "XProtect", "syspolicyd"}},
	{"Endpoint security / antivirus agent", "A security agent inspects file and process activity; that overhead lands on every app.",
		"If it's constantly busy, ask IT about exclusions for developer folders and build tools.",
		[]string{"wdavdaemon", "wdavdaemon_enterprise", "wdavdaemon_unprivileged", "falcond", "com.crowdstrike.falcon.Agent", "SentinelAgent", "sentineld", "CbOsxSensorService", "SophosScanD", "JamfDaemon", "jamf"}},
}

var daemonByName = func() map[string]*daemon {
	m := map[string]*daemon{}
	for i := range knownDaemons {
		for _, n := range knownDaemons[i].names {
			m[n] = &knownDaemons[i]
		}
	}
	return m
}()
