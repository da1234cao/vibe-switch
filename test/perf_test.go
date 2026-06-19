package test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"vibe-switch/internal/harness"
)

// Performance suite. Default behavior is to REPORT numbers (t.Log); it only
// asserts when the corresponding env var is set, so it never goes red just from
// running on a slower machine:
//
//	PERF_MIN_PPS     minimum 64B full-speed forwarding rate (pps)
//	PERF_MAX_LOSS    maximum 64B full-speed loss (percent)
//	PERF_MAX_P99_US  maximum one-way p99 latency (microseconds)
//
// Numbers are RELATIVE (software veth path, no real NIC) — meant for comparing
// the Go switch against the Linux bridge on the same machine, not absolute.

func perfTopology(t *testing.T) *harness.Topology {
	top := newTopology(t, plainHosts(2))
	// Pre-learn h2 so traffic is known-unicast single-port forwarding.
	top.Inject(2, harness.BuildFrame(top.MAC(2), harness.Broadcast, []byte("learn"), 0))
	time.Sleep(100 * time.Millisecond)
	return top
}

func TestPerfThroughput(t *testing.T) {
	top := perfTopology(t)
	var pps64, loss64 float64
	for _, size := range []int{64, 512, 1500} {
		r := top.Throughput(1, 2, size, 800*time.Millisecond, 0)
		t.Logf("[%s] throughput %4dB: %.0f pps, %.1f Mbit/s, tx=%d rx=%d loss=%.2f%%",
			top.Switch().Name(), size, r.PPS(), r.Mbps(), r.TX, r.RX, r.LossPct())
		if size == 64 {
			pps64, loss64 = r.PPS(), r.LossPct()
		}
	}
	if v, ok := envFloat("PERF_MIN_PPS"); ok && pps64 < v {
		t.Errorf("64B pps %.0f below PERF_MIN_PPS %.0f", pps64, v)
	}
	if v, ok := envFloat("PERF_MAX_LOSS"); ok && loss64 > v {
		t.Errorf("64B loss %.2f%% above PERF_MAX_LOSS %.2f%%", loss64, v)
	}
}

func TestPerfLatency(t *testing.T) {
	top := perfTopology(t)
	lat := top.Latency(1, 2, 400, 1*time.Millisecond)
	if len(lat) == 0 {
		t.Fatal("no latency samples captured")
	}
	us := func(ns int64) float64 { return float64(ns) / 1000 }
	p50 := harness.Percentile(lat, 50)
	p99 := harness.Percentile(lat, 99)
	t.Logf("[%s] latency over %d samples: min=%.1fus p50=%.1fus p99=%.1fus max=%.1fus",
		top.Switch().Name(), len(lat), us(lat[0]), us(p50), us(p99), us(lat[len(lat)-1]))

	if v, ok := envFloat("PERF_MAX_P99_US"); ok && us(p99) > v {
		t.Errorf("p99 latency %.1fus above PERF_MAX_P99_US %.1fus", us(p99), v)
	}
}

func TestPerfStressLoss(t *testing.T) {
	top := perfTopology(t)
	for _, pps := range []int{10_000, 50_000, 100_000} {
		r := top.Throughput(1, 2, 64, 500*time.Millisecond, pps)
		t.Logf("[%s] stress @%6d pps target: delivered %.0f pps, loss=%.2f%%",
			top.Switch().Name(), pps, r.PPS(), r.LossPct())
	}
}

func envFloat(key string) (float64, bool) {
	s := os.Getenv(key)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
