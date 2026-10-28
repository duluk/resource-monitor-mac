package main

import (
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// latencyProbe measures how late a user-interactive thread wakes up from a
// short sleep. On an unloaded Mac that's tens of microseconds; when the CPU is
// oversubscribed or the thread's pages have to come back from swap it grows to
// milliseconds — the same delay a UI thread sees before it can handle a key press.
type latencyProbe struct {
	mu      sync.Mutex
	samples []float64 // overshoot in ms
	stop    atomic.Bool
	done    chan struct{}
}

func startLatencyProbe(sleep time.Duration) *latencyProbe {
	p := &latencyProbe{done: make(chan struct{})}
	go func() {
		// Never unlocked: the thread exits with this goroutine, taking its raised QoS with it.
		runtime.LockOSThread()
		defer close(p.done)
		setInteractiveQoS()
		us := int(sleep / time.Microsecond)
		buf := make([]uint64, max(1, int(100*time.Millisecond/sleep)))
		for !p.stop.Load() {
			latencyBatch(buf, us)
			p.mu.Lock()
			for _, v := range buf {
				p.samples = append(p.samples, float64(v)/1e6)
			}
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *latencyProbe) drain() []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.samples
	p.samples = nil
	return s
}

func (p *latencyProbe) stopAndDrain() []float64 {
	p.stop.Store(true)
	<-p.done
	return p.drain()
}

type LatencyStats struct {
	Samples   int
	P50Ms     float64
	P95Ms     float64
	P99Ms     float64
	MaxMs     float64
	Over16Ms  int // longer than one 60 Hz frame
	Over100Ms int // clearly perceptible stall
}

func latencyStats(samples []float64) LatencyStats {
	st := LatencyStats{Samples: len(samples)}
	if len(samples) == 0 {
		return st
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	pct := func(q float64) float64 { return s[min(len(s)-1, int(q*float64(len(s))))] }
	st.P50Ms, st.P95Ms, st.P99Ms, st.MaxMs = pct(0.50), pct(0.95), pct(0.99), s[len(s)-1]
	for _, v := range s {
		if v > 16 {
			st.Over16Ms++
		}
		if v > 100 {
			st.Over100Ms++
		}
	}
	return st
}
