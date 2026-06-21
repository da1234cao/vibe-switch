package goswitch

import (
	"net"
	"sync/atomic"
	"time"
)

// portStats are per-port counters bumped on the forwarding hot path. All fields
// are accessed with sync/atomic, so reads (snapshots) never block forwarding.
type portStats struct {
	rxFrames    uint64
	rxBytes     uint64
	txFrames    uint64
	txBytes     uint64
	flooded     uint64
	forwardDrop uint64 // ingress: unparseable frame or illegal VLAN tag for the role
	txDrop      uint64 // egress: WritePacketData failed (e.g. frame > MTU → EMSGSIZE)
}

// --- read-only snapshots for the management interface ---
//
// These are plain gob-encodable structs (no locks/pointers) so the control
// package can return them straight over net/rpc.

type FDBEntry struct {
	VID        uint16
	MAC        string
	Port       string
	AgeSeconds float64
}

type PortInfo struct {
	Name  string
	Role  string
	PVID  uint16
	Trunk []uint16
	Up    bool
}

type PortStats struct {
	Name        string
	RxFrames    uint64
	RxBytes     uint64
	TxFrames    uint64
	TxBytes     uint64
	Flooded     uint64
	ForwardDrop uint64
	TxDrop      uint64
}

type EngineConfig struct {
	AgeingSeconds float64
	NumPorts      int
	VLANAware     bool
}

// FDBSnapshot copies the learning table under the read lock.
func (e *Engine) FDBSnapshot() []FDBEntry {
	now := time.Now().UnixNano()
	e.fdb.mu.RLock()
	out := make([]FDBEntry, 0, len(e.fdb.table))
	for k, en := range e.fdb.table {
		mac := net.HardwareAddr(k.mac[:])
		out = append(out, FDBEntry{
			VID:        k.vid,
			MAC:        mac.String(),
			Port:       e.ports[en.portIdx].name,
			AgeSeconds: float64(now-atomic.LoadInt64(&en.seenAt)) / float64(time.Second),
		})
	}
	e.fdb.mu.RUnlock()
	return out
}

func (e *Engine) PortsSnapshot() []PortInfo {
	out := make([]PortInfo, 0, len(e.ports))
	for _, p := range e.ports {
		out = append(out, PortInfo{
			Name:  p.name,
			Role:  p.role.String(),
			PVID:  p.pvid,
			Trunk: p.trunkVIDs,
			Up:    true,
		})
	}
	return out
}

func (e *Engine) StatsSnapshot() []PortStats {
	out := make([]PortStats, 0, len(e.ports))
	for _, p := range e.ports {
		out = append(out, PortStats{
			Name:        p.name,
			RxFrames:    atomic.LoadUint64(&p.stats.rxFrames),
			RxBytes:     atomic.LoadUint64(&p.stats.rxBytes),
			TxFrames:    atomic.LoadUint64(&p.stats.txFrames),
			TxBytes:     atomic.LoadUint64(&p.stats.txBytes),
			Flooded:     atomic.LoadUint64(&p.stats.flooded),
			ForwardDrop: atomic.LoadUint64(&p.stats.forwardDrop),
			TxDrop:      atomic.LoadUint64(&p.stats.txDrop),
		})
	}
	return out
}

func (e *Engine) ConfigSnapshot() EngineConfig {
	return EngineConfig{
		AgeingSeconds: float64(e.fdb.ageing.Load()) / float64(time.Second),
		NumPorts:      len(e.ports),
		VLANAware:     e.vlanAware,
	}
}
