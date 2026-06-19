package harness

import (
	"bytes"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/vishvananda/netns"
	"golang.org/x/net/bpf"
)

// TestEtherType marks frames crafted by the harness (IEEE local experimental
// EtherType 1). Lets captures ignore any stray traffic on the wire.
const TestEtherType layers.EthernetType = 0x88B5

// magic prefixes every test payload as a second, belt-and-suspenders marker.
var magic = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// openInNetns creates an AF_PACKET handle on ifname inside network namespace
// nsName. It enters the namespace only for the socket() call: the returned
// handle's fd is independent of the calling thread's namespace afterwards, so
// reads/writes happen freely outside any namespace context.
func openInNetns(nsName string, opts ...interface{}) (*afpacket.TPacket, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread() // safe: we restore the original ns before unlocking

	orig, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer orig.Close()

	target, err := netns.GetFromName(nsName)
	if err != nil {
		return nil, err
	}
	defer target.Close()

	if err := netns.Set(target); err != nil {
		return nil, err
	}
	defer netns.Set(orig) // restore (runs before UnlockOSThread)

	return afpacket.NewTPacket(opts...)
}

// inboundFilter drops PACKET_OUTGOING frames so a handle that both injects and
// captures on the same interface never sees its own transmissions (which would
// otherwise masquerade as a reflection/loopback).
func inboundFilter() []bpf.RawInstruction {
	raw, err := bpf.Assemble([]bpf.Instruction{
		bpf.LoadExtension{Num: bpf.ExtType},                  // A = skb->pkt_type
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: 4, SkipTrue: 1}, // PACKET_OUTGOING == 4 → drop
		bpf.RetConstant{Val: 0x40000},                        // accept
		bpf.RetConstant{Val: 0},                              // drop
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func captureOpts(ifname string) []interface{} {
	return []interface{}{
		afpacket.OptInterface(ifname),
		afpacket.OptFrameSize(4096),
		afpacket.OptBlockSize(4096 * 8),
		afpacket.OptNumBlocks(8),
		afpacket.OptBlockTimeout(10 * time.Millisecond),
		afpacket.OptPollTimeout(10 * time.Millisecond),
		// Reconstruct the 802.1Q tag into the frame bytes — the kernel often
		// strips it into tpacket aux data, which would hide VLAN tags from us.
		afpacket.OptAddVLANHeader(true),
	}
}

// --- frame construction ---

// BuildFrame builds an untagged Ethernet test frame. If padTo > 0 the payload
// is zero-padded so the total frame is at least padTo bytes.
func BuildFrame(src, dst net.HardwareAddr, payload []byte, padTo int) []byte {
	return serialize(src, dst, 0, payload, padTo)
}

// BuildVLANFrame builds an 802.1Q-tagged Ethernet test frame.
func BuildVLANFrame(src, dst net.HardwareAddr, vid uint16, payload []byte, padTo int) []byte {
	return serialize(src, dst, vid, payload, padTo)
}

func serialize(src, dst net.HardwareAddr, vid uint16, payload []byte, padTo int) []byte {
	body := append(append([]byte{}, magic...), payload...)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true}
	eth := &layers.Ethernet{SrcMAC: src, DstMAC: dst, EthernetType: TestEtherType}
	if vid != 0 {
		eth.EthernetType = layers.EthernetTypeDot1Q
		dot1q := &layers.Dot1Q{VLANIdentifier: vid, Type: TestEtherType}
		_ = gopacket.SerializeLayers(buf, opts, eth, dot1q, gopacket.Payload(body))
	} else {
		_ = gopacket.SerializeLayers(buf, opts, eth, gopacket.Payload(body))
	}
	frame := buf.Bytes()
	if padTo > len(frame) {
		frame = append(frame, make([]byte, padTo-len(frame))...)
	}
	return frame
}

// --- frame parsing ---

// FrameInfo is the decoded view of a captured frame.
type FrameInfo struct {
	Src, Dst net.HardwareAddr
	HasVLAN  bool
	VLANID   uint16
	EthType  layers.EthernetType // inner type when tagged
	Payload  []byte
}

// isTest reports whether the frame was crafted by this harness.
func (fi FrameInfo) isTest() bool {
	return fi.EthType == TestEtherType && len(fi.Payload) >= 4 && bytes.Equal(fi.Payload[:4], magic)
}

// Parse decodes an Ethernet (optionally 802.1Q) frame.
func Parse(b []byte) (FrameInfo, bool) {
	pkt := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.NoCopy)
	el := pkt.Layer(layers.LayerTypeEthernet)
	if el == nil {
		return FrameInfo{}, false
	}
	e := el.(*layers.Ethernet)
	fi := FrameInfo{Src: e.SrcMAC, Dst: e.DstMAC, EthType: e.EthernetType, Payload: e.Payload}
	if dl := pkt.Layer(layers.LayerTypeDot1Q); dl != nil {
		dq := dl.(*layers.Dot1Q)
		fi.HasVLAN = true
		fi.VLANID = dq.VLANIdentifier
		fi.EthType = dq.Type
		fi.Payload = dq.Payload
	}
	return fi, true
}

// --- injection ---

// Inject sends a single raw frame from host hostIdx.
func (top *Topology) Inject(hostIdx int, frame []byte) {
	h := top.Host(hostIdx)
	tp, err := openInNetns(h.NS, afpacket.OptInterface(h.IfName))
	if err != nil {
		top.t.Fatalf("inject open on %s: %v", h.NS, err)
	}
	defer tp.Close()
	if err := tp.WritePacketData(frame); err != nil {
		top.t.Fatalf("inject write on %s: %v", h.NS, err)
	}
}

// --- capture ---

type capture struct {
	tp       *afpacket.TPacket
	mu       sync.Mutex
	frames   [][]byte
	done     chan struct{}
	finished chan struct{}
}

func (c *capture) loop() {
	defer close(c.finished)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		data, _, err := c.tp.ZeroCopyReadPacketData()
		if err == afpacket.ErrTimeout {
			continue
		}
		if err != nil {
			return
		}
		b := make([]byte, len(data)) // zero-copy buffer is reused; copy out
		copy(b, data)
		c.mu.Lock()
		c.frames = append(c.frames, b)
		c.mu.Unlock()
	}
}

// Captures is a set of concurrent per-host capture handles, all armed before
// the caller injects, so no flooded frame is missed.
type Captures struct {
	caps map[int]*capture
}

// Capture opens and arms inbound captures on the given host indices.
func (top *Topology) Capture(hostIdxs ...int) *Captures {
	cs := &Captures{caps: make(map[int]*capture)}
	for _, idx := range hostIdxs {
		h := top.Host(idx)
		tp, err := openInNetns(h.NS, captureOpts(h.IfName)...)
		if err != nil {
			top.t.Fatalf("capture open on %s: %v", h.NS, err)
		}
		if err := tp.SetBPF(inboundFilter()); err != nil {
			top.t.Fatalf("capture setbpf on %s: %v", h.NS, err)
		}
		c := &capture{tp: tp, done: make(chan struct{}), finished: make(chan struct{})}
		go c.loop()
		cs.caps[idx] = c
	}
	return cs
}

// Close stops every capture loop and releases the handles.
func (cs *Captures) Close() {
	for _, c := range cs.caps {
		close(c.done)
		<-c.finished
		c.tp.Close()
	}
}

// Received returns the harness-crafted frames captured on host idx.
func (cs *Captures) Received(idx int) []FrameInfo {
	c := cs.caps[idx]
	c.mu.Lock()
	raw := append([][]byte(nil), c.frames...)
	c.mu.Unlock()

	var out []FrameInfo
	for _, b := range raw {
		if fi, ok := Parse(b); ok && fi.isTest() {
			out = append(out, fi)
		}
	}
	return out
}

// Count is the number of harness frames captured on host idx.
func (cs *Captures) Count(idx int) int { return len(cs.Received(idx)) }
