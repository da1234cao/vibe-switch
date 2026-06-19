package harness

import (
	"encoding/binary"
	"sort"
	"time"

	"github.com/gopacket/gopacket/afpacket"
	"golang.org/x/net/bpf"
)

// perfStart is a fixed monotonic reference. Both the sender (embeds nowNanos in
// the payload) and the latency receiver (reads nowNanos on arrival) measure
// from it, so one-way latency is valid without any clock synchronization.
var perfStart = time.Now()

func nowNanos() int64 { return int64(time.Since(perfStart)) }

// perfBPF keeps only inbound harness frames (untagged TestEtherType), so the
// high-rate counter ignores everything else cheaply in the kernel.
func perfBPF() []bpf.RawInstruction {
	raw, err := bpf.Assemble([]bpf.Instruction{
		bpf.LoadExtension{Num: bpf.ExtType},                       // 0: A = pkt_type
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 4, SkipTrue: 4},      // 1: OUTGOING → drop (idx6)
		bpf.LoadAbsolute{Off: 12, Size: 2},                        // 2: A = ethertype
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(TestEtherType), SkipTrue: 1}, // 3: match → accept (idx5)
		bpf.RetConstant{Val: 0},                                   // 4: drop
		bpf.RetConstant{Val: 0x40000},                             // 5: accept
		bpf.RetConstant{Val: 0},                                   // 6: drop
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// --- sender ---

type sender struct{ tp *afpacket.TPacket }

func (top *Topology) newSender(idx int) *sender {
	h := top.Host(idx)
	tp, err := openInNetns(h.NS,
		afpacket.OptInterface(h.IfName),
		afpacket.OptFrameSize(2048),
		afpacket.OptBlockSize(4096),
		afpacket.OptNumBlocks(4),
		afpacket.OptBlockTimeout(10*time.Millisecond),
		afpacket.OptPollTimeout(10*time.Millisecond),
	)
	if err != nil {
		top.t.Fatalf("sender open on %s: %v", h.NS, err)
	}
	return &sender{tp: tp}
}

func (s *sender) close() { s.tp.Close() }

// --- counter (high-rate RX, counts only) ---

type counter struct {
	tp       *afpacket.TPacket
	n        int64
	done     chan struct{}
	finished chan struct{}
}

func (top *Topology) newCounter(idx int) *counter {
	h := top.Host(idx)
	tp, err := openInNetns(h.NS,
		afpacket.OptInterface(h.IfName),
		afpacket.OptPollTimeout(20*time.Millisecond), // big default ring, just bound poll
	)
	if err != nil {
		top.t.Fatalf("counter open on %s: %v", h.NS, err)
	}
	if err := tp.SetBPF(perfBPF()); err != nil {
		top.t.Fatalf("counter setbpf on %s: %v", h.NS, err)
	}
	c := &counter{tp: tp, done: make(chan struct{}), finished: make(chan struct{})}
	go c.loop()
	return c
}

func (c *counter) loop() {
	defer close(c.finished)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		_, _, err := c.tp.ZeroCopyReadPacketData()
		if err == afpacket.ErrTimeout {
			continue
		}
		if err != nil {
			return
		}
		c.n++ // single reader goroutine; read after close(done)+<-finished
	}
}

func (c *counter) stop() int64 {
	close(c.done)
	<-c.finished
	n := c.n
	c.tp.Close()
	return n
}

// --- throughput ---

// ThroughputResult holds one throughput run.
type ThroughputResult struct {
	FrameSize int
	TX, RX    int64
	Elapsed   time.Duration
}

func (r ThroughputResult) PPS() float64  { return float64(r.RX) / r.Elapsed.Seconds() }
func (r ThroughputResult) Mbps() float64 {
	return float64(r.RX) * float64(r.FrameSize) * 8 / 1e6 / r.Elapsed.Seconds()
}
func (r ThroughputResult) LossPct() float64 {
	if r.TX == 0 {
		return 0
	}
	return float64(r.TX-r.RX) / float64(r.TX) * 100
}

// Throughput blasts known-unicast frames srcIdx→dstIdx for d. If targetPPS > 0
// the send rate is capped (paced per-millisecond); 0 means full speed. The
// caller must have pre-learned dstIdx's MAC so traffic is single-port unicast.
func (top *Topology) Throughput(srcIdx, dstIdx, frameSize int, d time.Duration, targetPPS int) ThroughputResult {
	s := top.newSender(srcIdx)
	c := top.newCounter(dstIdx)
	frame := BuildFrame(top.MAC(srcIdx), top.MAC(dstIdx), make([]byte, 64), frameSize)

	// Warm up (and re-learn) without measuring. Drain long enough that the
	// counter goroutine has fully accounted the warmup before we snapshot, so
	// these frames are excluded from the measured window.
	for i := 0; i < 200; i++ {
		_ = s.tp.WritePacketData(frame)
	}
	time.Sleep(250 * time.Millisecond)

	cStart := c.n
	var tx int64
	start := time.Now()
	if targetPPS <= 0 {
		for time.Since(start) < d {
			if s.tp.WritePacketData(frame) == nil {
				tx++
			}
		}
	} else {
		perMs := targetPPS / 1000
		if perMs < 1 {
			perMs = 1
		}
		for tick := time.Now(); time.Since(start) < d; {
			for i := 0; i < perMs; i++ {
				if s.tp.WritePacketData(frame) == nil {
					tx++
				}
			}
			tick = tick.Add(time.Millisecond)
			if dt := time.Until(tick); dt > 0 {
				time.Sleep(dt)
			}
		}
	}
	elapsed := time.Since(start)

	time.Sleep(150 * time.Millisecond) // drain the ring
	rx := c.stop() - cStart
	s.close()
	return ThroughputResult{FrameSize: frameSize, TX: tx, RX: rx, Elapsed: elapsed}
}

// --- latency ---

// Latency sends n timestamped frames srcIdx→dstIdx at the given interval and
// returns the per-frame one-way latencies (nanoseconds), sorted ascending.
func (top *Topology) Latency(srcIdx, dstIdx, n int, interval time.Duration) []int64 {
	s := top.newSender(srcIdx)
	h := top.Host(dstIdx)
	// TPACKET_V2 with a 1ms block timeout delivers frames promptly, one at a
	// time — V3's default 64ms block aggregation would otherwise dominate the
	// measured latency.
	rx, err := openInNetns(h.NS,
		afpacket.OptInterface(h.IfName),
		afpacket.OptTPacketVersion(afpacket.TPacketVersion2),
		afpacket.OptFrameSize(2048),
		afpacket.OptBlockSize(4096),
		afpacket.OptNumBlocks(64),
		afpacket.OptBlockTimeout(1*time.Millisecond),
		afpacket.OptPollTimeout(2*time.Millisecond),
	)
	if err != nil {
		top.t.Fatalf("latency rx open on %s: %v", h.NS, err)
	}
	if err := rx.SetBPF(perfBPF()); err != nil {
		top.t.Fatalf("latency rx setbpf: %v", err)
	}

	var lat []int64
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
			}
			data, _, err := rx.ZeroCopyReadPacketData()
			if err == afpacket.ErrTimeout {
				continue
			}
			if err != nil {
				return
			}
			recv := nowNanos()
			fi, ok := Parse(data)
			if !ok || !fi.isTest() || len(fi.Payload) < 20 {
				continue
			}
			sent := int64(binary.BigEndian.Uint64(fi.Payload[12:20]))
			lat = append(lat, recv-sent)
		}
	}()

	srcMAC, dstMAC := top.MAC(srcIdx), top.MAC(dstIdx)
	payload := make([]byte, 16)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(payload[0:8], uint64(i))
		binary.BigEndian.PutUint64(payload[8:16], uint64(nowNanos()))
		_ = s.tp.WritePacketData(BuildFrame(srcMAC, dstMAC, payload, 0))
		time.Sleep(interval)
	}
	time.Sleep(150 * time.Millisecond) // drain
	close(done)
	<-finished
	rx.Close()
	s.close()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat
}

// Percentile returns the p-th percentile (0..100) of a sorted slice.
func Percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
