package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// VMStat holds page counts from host_statistics64 (counters are cumulative since boot).
type VMStat struct {
	PageSize, Free, Active, Inactive, Wired, Speculative, Compressor, Purgeable,
	External, Internal, UncompressedInCompressor uint64
	Pageins, Pageouts, Swapins, Swapouts, Compressions, Decompressions, Faults uint64
}

type CPUTicks struct{ User, Sys, Idle, Nice uint64 }

type DiskStats struct {
	OK                                                        bool
	ReadBytes, WriteBytes, ReadOps, WriteOps, ReadNs, WriteNs uint64
}

// Proc is one process at one instant. CPU, disk and page-in counters are cumulative.
type Proc struct {
	PID, PPID, RespPID, UID int
	Path, Name              string
	// Full is false when the kernel refused detailed stats (another user's
	// process without sudo); CPU and memory then come from ps (RSS, not footprint).
	Full                                                       bool
	CPUNs, Footprint, Resident, DiskR, DiskW, Pageins, Wakeups uint64
}

// SysSample is a cheap system-wide snapshot taken every tick.
type SysSample struct {
	At                  time.Time
	VM                  VMStat
	CPU                 CPUTicks
	SwapTotal, SwapUsed uint64
	PressureLevel       int // kern.memorystatus_vm_pressure_level: 1 normal, 2 warn, 4 critical
	FreeLevel           int // kern.memorystatus_level: % of memory the kernel considers available
	Disk                DiskStats
	GPU                 int // instantaneous %, -1 if unknown
	Thermal             int
	Load                [3]float64
}

type SystemInfo struct {
	Model, CPU, OSVersion string
	NCPU, PCores, ECores  int
	MemBytes              uint64
	Boot                  time.Time
	DiskTotal, DiskFree   uint64
	LowPowerMode          bool
	Root                  bool
}

func readSystemInfo() SystemInfo {
	si := SystemInfo{
		Model:        sysctlString("hw.model"),
		CPU:          sysctlString("machdep.cpu.brand_string"),
		OSVersion:    sysctlString("kern.osproductversion"),
		NCPU:         int(sysctlInt("hw.ncpu")),
		PCores:       int(sysctlInt("hw.perflevel0.physicalcpu")),
		ECores:       int(sysctlInt("hw.perflevel1.physicalcpu")),
		MemBytes:     uint64(sysctlInt("hw.memsize")),
		Boot:         bootTime(),
		LowPowerMode: lowPowerMode(),
		Root:         syscall.Geteuid() == 0,
	}
	for _, mnt := range []string{"/System/Volumes/Data", "/"} {
		var fs syscall.Statfs_t
		if syscall.Statfs(mnt, &fs) == nil {
			si.DiskTotal = fs.Blocks * uint64(fs.Bsize)
			si.DiskFree = fs.Bavail * uint64(fs.Bsize)
			break
		}
	}
	return si
}

func sampleSystem() SysSample {
	s := SysSample{At: time.Now()}
	s.VM, _ = readVM()
	s.CPU = readCPUTicks()
	s.SwapTotal, s.SwapUsed = readSwap()
	s.PressureLevel = int(sysctlInt("kern.memorystatus_vm_pressure_level"))
	s.FreeLevel = int(sysctlInt("kern.memorystatus_level"))
	s.Disk = readDisk()
	s.GPU = readGPU()
	s.Thermal = thermalState()
	s.Load = loadAvg()
	return s
}

// fillFromPS covers processes the kernel wouldn't give us details for (other
// users' processes when not root): ps can still report their CPU time and RSS.
func fillFromPS(procs map[int]*Proc) {
	out, err := exec.Command("ps", "-axo", "pid=,rss=,cputime=").Output()
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		p, ok := procs[pid]
		if !ok || p.Full {
			continue
		}
		rss, _ := strconv.ParseUint(f[1], 10, 64)
		p.Resident = rss * 1024
		p.Footprint = p.Resident
		p.CPUNs = parseCPUTime(f[2])
	}
}

// parseCPUTime parses ps time formats like "12:34.56", "1:02:03.45" or "2-01:02:03".
func parseCPUTime(s string) uint64 {
	var days float64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.ParseFloat(s[:i], 64)
		s = s[i+1:]
	}
	secs := 0.0
	for part := range strings.SplitSeq(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		secs = secs*60 + v
	}
	secs += days * 86400
	return uint64(secs * 1e9)
}
