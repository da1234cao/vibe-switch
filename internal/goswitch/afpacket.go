package goswitch

import (
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"golang.org/x/net/bpf"
)

// PacketIO is the minimal frame transport the Engine needs from a port. It is
// satisfied as-is by *afpacket.TPacket, so the Engine never imports afpacket and
// can be unit-tested against a fake.
type PacketIO interface {
	ReadPacketData() (data []byte, ci gopacket.CaptureInfo, err error)
	WritePacketData(pkt []byte) error
	Close()
}

// RxOpts is the shared AF_PACKET option set for switch ports, used by both the
// CLI (current netns) and the test harness adapter (inside the switch netns).
//
// OptAddVLANHeader is always on: the kernel otherwise strips the 802.1Q tag into
// tpacket aux data, hiding it from us — fatal for trunk ports. It is harmless on
// untagged (plain/access) frames.
func RxOpts(ifname string) []interface{} {
	return []interface{}{
		afpacket.OptInterface(ifname),
		afpacket.OptFrameSize(4096),
		afpacket.OptBlockSize(4096 * 32),
		afpacket.OptNumBlocks(16),
		afpacket.OptBlockTimeout(1 * time.Millisecond),
		afpacket.OptPollTimeout(1 * time.Millisecond),
		afpacket.OptAddVLANHeader(true),
	}
}

// InboundBPF drops PACKET_OUTGOING frames so a port never reads back the frames
// the switch itself transmits on it (which would otherwise loop). Same filter as
// the harness probe uses for captures.
func InboundBPF() []bpf.RawInstruction {
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

// rxLoop reads frames from a port and forwards them until shutdown. It lives
// here (not engine.go) so the engine core stays free of the afpacket import; it
// needs afpacket.ErrTimeout to tell an idle poll-timeout apart from a closed
// handle.
func (e *Engine) rxLoop(p *port) {
	defer e.wg.Done()
	for {
		select {
		case <-e.done:
			return
		default:
		}
		data, _, err := p.io.ReadPacketData()
		if err == afpacket.ErrTimeout {
			continue // idle poll cycle; loop back to re-check done
		}
		if err != nil {
			return // handle closed (Stop) or fatal
		}
		e.forward(p, data)
	}
}

// OpenInterface opens an AF_PACKET handle on ifname in the current network
// namespace and installs the inbound filter. Used by the standalone binary; the
// test harness opens its handles inside the switch netns instead.
func OpenInterface(ifname string) (*afpacket.TPacket, error) {
	tp, err := afpacket.NewTPacket(RxOpts(ifname)...)
	if err != nil {
		return nil, err
	}
	if err := tp.SetBPF(InboundBPF()); err != nil {
		tp.Close()
		return nil, err
	}
	return tp, nil
}
