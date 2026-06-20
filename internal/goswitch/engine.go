// Package goswitch is a user-space L2/VLAN learning switch forwarding engine.
//
// It is decoupled from I/O via the PacketIO interface, so the same engine drives
// both the behavioral test harness (SWITCH=goswitch, handles opened inside a
// netns) and the standalone vibe-switch binary (handles on real host interfaces).
// The engine does MAC learning, known-unicast forwarding, unknown/broadcast
// flooding, split-horizon (no return to ingress), MAC ageing, and VLAN
// access/trunk tag handling. Frames are parsed and re-serialized with gopacket.
package goswitch

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

// PortConfig describes one switch port. AccessVID/Trunk mirror the harness Port
// contract: AccessVID!=0 (Trunk empty) → access; Trunk non-empty → trunk; both
// zero/empty → plain (transparent L2).
type PortConfig struct {
	Name      string
	AccessVID uint16
	Trunk     []uint16
}

type port struct {
	idx       int
	name      string
	io        PacketIO
	role      portRole
	pvid      uint16          // access PVID
	allow     map[uint16]bool // trunk allowed VIDs
	trunkVIDs []uint16        // trunk VIDs in config order (for snapshots)
	writeMu   sync.Mutex      // serializes WritePacketData on this port
	stats     portStats
	parser    *frameParser
}

// frameParser is a zero-allocation gopacket decoder, one per rx goroutine (so no
// sharing/contention on the reused layer structs).
type frameParser struct {
	eth   layers.Ethernet
	dot1q layers.Dot1Q
	dlp   *gopacket.DecodingLayerParser
	dec   []gopacket.LayerType
}

func newFrameParser() *frameParser {
	fp := &frameParser{}
	fp.dlp = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &fp.eth, &fp.dot1q)
	fp.dlp.IgnoreUnsupported = true // stop at the first opaque payload, don't error
	return fp
}

// frameView is the decoded view used to make forwarding/egress decisions.
type frameView struct {
	dst, src  net.HardwareAddr
	hasTag    bool
	tagVID    uint16
	innerType layers.EthernetType // ethertype after eth (or after the tag)
	payload   []byte              // bytes after eth (or after the tag)
	raw       []byte              // original frame, for tag-preserving passthrough
}

// Engine is the forwarding core.
type Engine struct {
	ports      []*port
	fdb        *FDB
	vidMembers map[uint16][]int
	vlanAware  bool
	done       chan struct{}
	wg         sync.WaitGroup
	stopOnce   sync.Once
}

// NewEngine validates the config and wires ports to their I/O handles. ios[i]
// belongs to cfg[i]. It does not start any goroutines (call Start).
func NewEngine(cfg []PortConfig, ios []PacketIO) (*Engine, error) {
	if len(cfg) != len(ios) {
		return nil, fmt.Errorf("goswitch: %d port configs but %d handles", len(cfg), len(ios))
	}
	if len(cfg) == 0 {
		return nil, errors.New("goswitch: no ports")
	}
	e := &Engine{fdb: newFDB(), vidMembers: make(map[uint16][]int), done: make(chan struct{})}
	seen := make(map[string]bool, len(cfg))
	anyVLAN, anyPlain := false, false
	for i, c := range cfg {
		if c.Name == "" {
			return nil, fmt.Errorf("goswitch: port %d has empty name", i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("goswitch: duplicate port %q", c.Name)
		}
		seen[c.Name] = true
		if c.AccessVID != 0 && len(c.Trunk) > 0 {
			return nil, fmt.Errorf("goswitch: port %q sets both access and trunk", c.Name)
		}
		p := &port{idx: i, name: c.Name, io: ios[i], role: roleOf(c.AccessVID, c.Trunk), parser: newFrameParser()}
		switch p.role {
		case rolePlain:
			anyPlain = true
		case roleAccess:
			anyVLAN = true
			p.pvid = c.AccessVID
		case roleTrunk:
			anyVLAN = true
			p.allow = make(map[uint16]bool, len(c.Trunk))
			for _, v := range c.Trunk {
				p.allow[v] = true
			}
			p.trunkVIDs = append([]uint16(nil), c.Trunk...)
		}
		e.ports = append(e.ports, p)
	}
	if anyVLAN && anyPlain {
		return nil, errors.New("goswitch: cannot mix plain and VLAN (access/trunk) ports; configure all ports one way")
	}
	e.vlanAware = anyVLAN

	for _, p := range e.ports {
		switch p.role {
		case rolePlain:
			e.vidMembers[0] = append(e.vidMembers[0], p.idx)
		case roleAccess:
			e.vidMembers[p.pvid] = append(e.vidMembers[p.pvid], p.idx)
		case roleTrunk:
			for v := range p.allow {
				e.vidMembers[v] = append(e.vidMembers[v], p.idx)
			}
		}
	}
	return e, nil
}

// Start launches one rx goroutine per port plus the ageing GC goroutine.
func (e *Engine) Start() error {
	for _, p := range e.ports {
		e.wg.Add(1)
		go e.rxLoop(p)
	}
	e.wg.Add(1)
	go e.gcLoop()
	return nil
}

// Stop signals shutdown, waits for all goroutines to drain, then closes the
// handles. Order matters: closing an afpacket handle while its rxLoop is mid-read
// unmaps the ring buffer under the reader and segfaults. rxLoop exits within one
// poll timeout (~1ms) of seeing done, so we wait first and close after. Safe to
// call more than once.
func (e *Engine) Stop() error {
	e.stopOnce.Do(func() {
		close(e.done)
		e.wg.Wait()
		for _, p := range e.ports {
			p.io.Close()
		}
	})
	return nil
}

// SetAgeing sets the MAC ageing duration (0 = never age out).
func (e *Engine) SetAgeing(d time.Duration) error {
	e.fdb.ageing.Store(int64(d))
	return nil
}

func (e *Engine) gcLoop() {
	defer e.wg.Done()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-t.C:
			e.fdb.reap(time.Now().UnixNano())
		}
	}
}

// forward processes one frame received on ingress port p.
func (e *Engine) forward(p *port, frame []byte) {
	atomic.AddUint64(&p.stats.rxFrames, 1)
	atomic.AddUint64(&p.stats.rxBytes, uint64(len(frame)))

	fp := p.parser
	_ = fp.dlp.DecodeLayers(frame, &fp.dec) // IgnoreUnsupported: err just means "stopped early"
	hasEth, hasTag := false, false
	for _, lt := range fp.dec {
		switch lt {
		case layers.LayerTypeEthernet:
			hasEth = true
		case layers.LayerTypeDot1Q:
			hasTag = true
		}
	}
	if !hasEth || len(fp.eth.DstMAC) != 6 || len(fp.eth.SrcMAC) != 6 {
		atomic.AddUint64(&p.stats.dropped, 1)
		return
	}

	fv := frameView{dst: fp.eth.DstMAC, src: fp.eth.SrcMAC, hasTag: hasTag, raw: frame}
	if hasTag {
		fv.tagVID = fp.dot1q.VLANIdentifier
		fv.innerType = fp.dot1q.Type
		fv.payload = fp.dot1q.Payload
	} else {
		fv.innerType = fp.eth.EthernetType
		fv.payload = fp.eth.Payload
	}

	vid, ok := p.classify(hasTag, fv.tagVID)
	if !ok {
		atomic.AddUint64(&p.stats.dropped, 1)
		return
	}

	now := time.Now().UnixNano()
	if fv.src[0]&0x01 == 0 { // don't learn from multicast/broadcast source
		var s [6]byte
		copy(s[:], fv.src)
		e.fdb.Learn(vid, s, p.idx, now)
	}

	if fv.dst[0]&0x01 == 0 { // unicast destination
		var d [6]byte
		copy(d[:], fv.dst)
		if out, ok := e.fdb.Lookup(vid, d, now); ok {
			if out != p.idx { // split-horizon
				e.emit(e.ports[out], &fv, vid)
			}
			return
		}
	}

	// broadcast / multicast / unknown unicast → flood within the VLAN.
	atomic.AddUint64(&p.stats.flooded, 1)
	for _, idx := range e.vidMembers[vid] {
		if idx == p.idx {
			continue
		}
		e.emit(e.ports[idx], &fv, vid)
	}
}

// emit sends fv out a port, adjusting the 802.1Q tag to the egress port's role.
func (e *Engine) emit(out *port, fv *frameView, vid uint16) {
	var pkt []byte
	outWantsTag := out.role == roleTrunk
	switch {
	case outWantsTag == fv.hasTag:
		pkt = fv.raw // tag state already matches → forward verbatim
	case outWantsTag:
		pkt = buildTagged(fv.dst, fv.src, vid, fv.innerType, fv.payload)
	default:
		pkt = buildUntagged(fv.dst, fv.src, fv.innerType, fv.payload)
	}
	out.writeMu.Lock()
	err := out.io.WritePacketData(pkt)
	out.writeMu.Unlock()
	if err == nil {
		atomic.AddUint64(&out.stats.txFrames, 1)
		atomic.AddUint64(&out.stats.txBytes, uint64(len(pkt)))
	}
}
