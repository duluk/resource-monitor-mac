package main

/*
#cgo CFLAGS: -O2 -Wno-deprecated-declarations
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation -framework Foundation

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <sys/sysctl.h>
#include <sys/time.h>
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <mach/mach_time.h>
#include <pthread.h>
#include <pthread/qos.h>
#include <IOKit/IOKitLib.h>
#include <CoreFoundation/CoreFoundation.h>

// Private but long-stable libSystem call: which app is "responsible" for a
// process (e.g. Safari for its WebKit content processes). Weak so we degrade
// gracefully if it ever disappears.
extern pid_t responsibility_get_pid_responsible_for_pid(pid_t) __attribute__((weak_import));

int thermal_state(void);
int low_power_mode(void);

static mach_port_t host_port(void) {
	static mach_port_t p = MACH_PORT_NULL;
	if (p == MACH_PORT_NULL) p = mach_host_self();
	return p;
}

// rusage CPU times are in mach absolute time units (not ns on Apple Silicon).
static double ticks_to_ns(void) {
	static double f = 0;
	if (f == 0) {
		mach_timebase_info_data_t tb;
		mach_timebase_info(&tb);
		f = (double)tb.numer / tb.denom;
	}
	return f;
}

typedef struct {
	uint64_t page_size, free, active, inactive, wired, speculative, compressor,
		purgeable, external, internal, uncompressed_in_compressor;
	uint64_t pageins, pageouts, swapins, swapouts, compressions, decompressions, faults;
} vmstat_t;

static int get_vmstat(vmstat_t *o) {
	vm_statistics64_data_t vs;
	mach_msg_type_number_t cnt = HOST_VM_INFO64_COUNT;
	if (host_statistics64(host_port(), HOST_VM_INFO64, (host_info64_t)&vs, &cnt) != KERN_SUCCESS) return -1;
	vm_size_t ps = 0;
	host_page_size(host_port(), &ps);
	o->page_size = ps;
	o->free = vs.free_count;
	o->active = vs.active_count;
	o->inactive = vs.inactive_count;
	o->wired = vs.wire_count;
	o->speculative = vs.speculative_count;
	o->compressor = vs.compressor_page_count;
	o->purgeable = vs.purgeable_count;
	o->external = vs.external_page_count;
	o->internal = vs.internal_page_count;
	o->uncompressed_in_compressor = vs.total_uncompressed_pages_in_compressor;
	o->pageins = vs.pageins;
	o->pageouts = vs.pageouts;
	o->swapins = vs.swapins;
	o->swapouts = vs.swapouts;
	o->compressions = vs.compressions;
	o->decompressions = vs.decompressions;
	o->faults = vs.faults;
	return 0;
}

static int get_cpu_ticks(uint64_t *user, uint64_t *sys, uint64_t *idle, uint64_t *nice) {
	host_cpu_load_info_data_t ci;
	mach_msg_type_number_t cnt = HOST_CPU_LOAD_INFO_COUNT;
	if (host_statistics(host_port(), HOST_CPU_LOAD_INFO, (host_info_t)&ci, &cnt) != KERN_SUCCESS) return -1;
	*user = ci.cpu_ticks[CPU_STATE_USER];
	*sys = ci.cpu_ticks[CPU_STATE_SYSTEM];
	*idle = ci.cpu_ticks[CPU_STATE_IDLE];
	*nice = ci.cpu_ticks[CPU_STATE_NICE];
	return 0;
}

static int get_swap(uint64_t *total, uint64_t *used) {
	struct xsw_usage x;
	size_t len = sizeof x;
	if (sysctlbyname("vm.swapusage", &x, &len, NULL, 0) != 0) return -1;
	*total = x.xsu_total;
	*used = x.xsu_used;
	return 0;
}

static int64_t sysctl_int(const char *name) {
	int64_t v = 0;
	size_t len = sizeof v;
	if (sysctlbyname(name, &v, &len, NULL, 0) != 0) return -1;
	if (len == sizeof(int32_t)) return (int64_t)*(int32_t *)&v;
	return v;
}

static int sysctl_str(const char *name, char *buf, size_t n) {
	size_t len = n;
	if (sysctlbyname(name, buf, &len, NULL, 0) != 0) return -1;
	return (int)len;
}

static void load_avg(double *a) {
	if (getloadavg(a, 3) != 3) a[0] = a[1] = a[2] = -1;
}

static int64_t boot_time(void) {
	struct timeval tv;
	size_t len = sizeof tv;
	if (sysctlbyname("kern.boottime", &tv, &len, NULL, 0) != 0) return -1;
	return tv.tv_sec;
}

typedef struct {
	int pid, ppid, rpid, uid, full;
	uint64_t cpu_ns, footprint, resident, disk_r, disk_w, pageins, wakeups;
	char path[PROC_PIDPATHINFO_MAXSIZE];
	char name[2 * MAXCOMLEN + 1];
} psample_t;

// proc_listallpids takes a buffer size in bytes and returns a pid count.
static int list_pids(int *buf, int cap) {
	return proc_listallpids(buf, cap * (int)sizeof(int));
}

static void sample_proc(int pid, psample_t *o) {
	memset(o, 0, sizeof *o);
	o->pid = pid;
	o->rpid = -1;
	o->uid = -1;
	if (proc_pidpath(pid, o->path, sizeof o->path) <= 0) o->path[0] = 0;
	proc_name(pid, o->name, sizeof o->name);
	struct proc_bsdshortinfo bi;
	if (proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &bi, sizeof bi) == sizeof bi) {
		o->ppid = bi.pbsi_ppid;
		o->uid = bi.pbsi_uid;
	}
	if (responsibility_get_pid_responsible_for_pid) o->rpid = responsibility_get_pid_responsible_for_pid(pid);
	struct rusage_info_v4 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ri) == 0) {
		o->full = 1;
		o->cpu_ns = (uint64_t)((double)(ri.ri_user_time + ri.ri_system_time) * ticks_to_ns());
		o->footprint = ri.ri_phys_footprint;
		o->resident = ri.ri_resident_size;
		o->disk_r = ri.ri_diskio_bytesread;
		o->disk_w = ri.ri_diskio_byteswritten;
		o->pageins = ri.ri_pageins;
		o->wakeups = ri.ri_interrupt_wkups + ri.ri_pkg_idle_wkups;
	}
}

static uint64_t cf_u64(CFDictionaryRef d, CFStringRef key) {
	CFTypeRef n = CFDictionaryGetValue(d, key);
	int64_t v = 0;
	if (n && CFGetTypeID(n) == CFNumberGetTypeID()) CFNumberGetValue((CFNumberRef)n, kCFNumberSInt64Type, &v);
	return (uint64_t)v;
}

typedef struct {
	uint64_t read_bytes, write_bytes, read_ops, write_ops, read_ns, write_ns;
} diskstat_t;

// Sums the cumulative counters of every block storage driver (physical disks).
static int get_disk(diskstat_t *o) {
	memset(o, 0, sizeof *o);
	io_iterator_t it;
	if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOBlockStorageDriver"), &it) != KERN_SUCCESS) return -1;
	io_registry_entry_t e;
	while ((e = IOIteratorNext(it))) {
		CFTypeRef s = IORegistryEntryCreateCFProperty(e, CFSTR("Statistics"), kCFAllocatorDefault, 0);
		if (s && CFGetTypeID(s) == CFDictionaryGetTypeID()) {
			CFDictionaryRef d = (CFDictionaryRef)s;
			o->read_bytes += cf_u64(d, CFSTR("Bytes (Read)"));
			o->write_bytes += cf_u64(d, CFSTR("Bytes (Write)"));
			o->read_ops += cf_u64(d, CFSTR("Operations (Read)"));
			o->write_ops += cf_u64(d, CFSTR("Operations (Write)"));
			o->read_ns += cf_u64(d, CFSTR("Total Time (Read)"));
			o->write_ns += cf_u64(d, CFSTR("Total Time (Write)"));
		}
		if (s) CFRelease(s);
		IOObjectRelease(e);
	}
	IOObjectRelease(it);
	return 0;
}

// Instantaneous GPU utilization (%) as reported by the accelerator driver, or -1.
static int gpu_util(void) {
	io_iterator_t it;
	int best = -1;
	if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOAccelerator"), &it) != KERN_SUCCESS) return -1;
	io_registry_entry_t e;
	while ((e = IOIteratorNext(it))) {
		CFTypeRef p = IORegistryEntryCreateCFProperty(e, CFSTR("PerformanceStatistics"), kCFAllocatorDefault, 0);
		if (p && CFGetTypeID(p) == CFDictionaryGetTypeID()) {
			CFTypeRef n = CFDictionaryGetValue((CFDictionaryRef)p, CFSTR("Device Utilization %"));
			if (n && CFGetTypeID(n) == CFNumberGetTypeID()) {
				int v = 0;
				CFNumberGetValue((CFNumberRef)n, kCFNumberIntType, &v);
				if (v > best) best = v;
			}
		}
		if (p) CFRelease(p);
		IOObjectRelease(e);
	}
	IOObjectRelease(it);
	return best;
}

// Run the calling thread at the same QoS as an app's main/UI thread, so the
// latency probe experiences what keyboard handling would.
static void set_interactive_qos(void) {
	pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0);
}

// Sleeps sleep_us repeatedly, recording how late (ns) each wakeup was.
static void latency_probe(uint64_t *out, int iters, int sleep_us) {
	double f = ticks_to_ns();
	uint64_t want = (uint64_t)sleep_us * 1000;
	for (int i = 0; i < iters; i++) {
		uint64_t t0 = mach_absolute_time();
		usleep(sleep_us);
		uint64_t el = (uint64_t)((double)(mach_absolute_time() - t0) * f);
		out[i] = el > want ? el - want : 0;
	}
}
*/
import "C"

import (
	"strings"
	"time"
	"unsafe"
)

func readVM() (VMStat, bool) {
	var v C.vmstat_t
	if C.get_vmstat(&v) != 0 {
		return VMStat{}, false
	}
	return VMStat{
		PageSize:                 uint64(v.page_size),
		Free:                     uint64(v.free),
		Active:                   uint64(v.active),
		Inactive:                 uint64(v.inactive),
		Wired:                    uint64(v.wired),
		Speculative:              uint64(v.speculative),
		Compressor:               uint64(v.compressor),
		Purgeable:                uint64(v.purgeable),
		External:                 uint64(v.external),
		Internal:                 uint64(v.internal),
		UncompressedInCompressor: uint64(v.uncompressed_in_compressor),
		Pageins:                  uint64(v.pageins),
		Pageouts:                 uint64(v.pageouts),
		Swapins:                  uint64(v.swapins),
		Swapouts:                 uint64(v.swapouts),
		Compressions:             uint64(v.compressions),
		Decompressions:           uint64(v.decompressions),
		Faults:                   uint64(v.faults),
	}, true
}

func readCPUTicks() CPUTicks {
	var u, s, i, n C.uint64_t
	if C.get_cpu_ticks(&u, &s, &i, &n) != 0 {
		return CPUTicks{}
	}
	return CPUTicks{User: uint64(u), Sys: uint64(s), Idle: uint64(i), Nice: uint64(n)}
}

func readSwap() (total, used uint64) {
	var t, u C.uint64_t
	if C.get_swap(&t, &u) != 0 {
		return 0, 0
	}
	return uint64(t), uint64(u)
}

func sysctlInt(name string) int64 {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	return int64(C.sysctl_int(cs))
}

func sysctlString(name string) string {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	buf := make([]byte, 512)
	n := C.sysctl_str(cs, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(string(buf[:n]), "\x00")
}

func bootTime() time.Time {
	s := int64(C.boot_time())
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

func readDisk() DiskStats {
	var d C.diskstat_t
	if C.get_disk(&d) != 0 {
		return DiskStats{}
	}
	return DiskStats{
		OK:         true,
		ReadBytes:  uint64(d.read_bytes),
		WriteBytes: uint64(d.write_bytes),
		ReadOps:    uint64(d.read_ops),
		WriteOps:   uint64(d.write_ops),
		ReadNs:     uint64(d.read_ns),
		WriteNs:    uint64(d.write_ns),
	}
}

func loadAvg() [3]float64 {
	var a [3]C.double
	C.load_avg(&a[0])
	return [3]float64{float64(a[0]), float64(a[1]), float64(a[2])}
}

func readGPU() int       { return int(C.gpu_util()) }
func thermalState() int  { return int(C.thermal_state()) }
func lowPowerMode() bool { return C.low_power_mode() != 0 }
func setInteractiveQoS() { C.set_interactive_qos() }
func latencyBatch(out []uint64, sleepUs int) {
	C.latency_probe((*C.uint64_t)(unsafe.Pointer(&out[0])), C.int(len(out)), C.int(sleepUs))
}

func listPIDs() []int {
	n := int(C.list_pids(nil, 0))
	if n <= 0 {
		return nil
	}
	buf := make([]C.int, n+256) // headroom for processes spawned in between
	n = int(C.list_pids(&buf[0], C.int(len(buf))))
	pids := make([]int, 0, n)
	for _, p := range buf[:n] {
		if p >= 0 { // pid 0 is kernel_task
			pids = append(pids, int(p))
		}
	}
	return pids
}

func sampleProcs() map[int]*Proc {
	var s C.psample_t
	out := make(map[int]*Proc)
	for _, pid := range listPIDs() {
		C.sample_proc(C.int(pid), &s)
		p := &Proc{
			PID:       pid,
			PPID:      int(s.ppid),
			RespPID:   int(s.rpid),
			UID:       int(s.uid),
			Path:      C.GoString(&s.path[0]),
			Name:      C.GoString(&s.name[0]),
			Full:      s.full != 0,
			CPUNs:     uint64(s.cpu_ns),
			Footprint: uint64(s.footprint),
			Resident:  uint64(s.resident),
			DiskR:     uint64(s.disk_r),
			DiskW:     uint64(s.disk_w),
			Pageins:   uint64(s.pageins),
			Wakeups:   uint64(s.wakeups),
		}
		out[pid] = p
	}
	fillFromPS(out)
	return out
}
