package goswitch

import (
	"sync"
	"sync/atomic"
)

// macKey identifies a learned address within a VLAN. Using a [6]byte array
// (not a slice/string) keeps it comparable and allocation-free as a map key.
type macKey struct {
	vid uint16
	mac [6]byte
}

type entry struct {
	portIdx int
	seenAt  int64 // unixnano of last refresh; atomic
}

// FDB is the forwarding database: a VLAN-scoped MAC learning table with
// concurrent access from per-port rx goroutines.
type FDB struct {
	mu     sync.RWMutex
	table  map[macKey]*entry
	ageing atomic.Int64 // ageing duration in ns; 0 = never age out
}

func newFDB() *FDB {
	return &FDB{table: make(map[macKey]*entry)}
}

// Learn records that mac (in vlan vid) lives behind port. The hot path —
// already learned on the same port — only refreshes the timestamp via atomic,
// avoiding the write lock entirely.
func (f *FDB) Learn(vid uint16, mac [6]byte, port int, now int64) {
	k := macKey{vid, mac}
	f.mu.RLock()
	e, ok := f.table[k]
	f.mu.RUnlock()
	if ok && e.portIdx == port {
		atomic.StoreInt64(&e.seenAt, now)
		return
	}
	f.mu.Lock()
	if e, ok := f.table[k]; ok {
		e.portIdx = port
		atomic.StoreInt64(&e.seenAt, now)
	} else {
		f.table[k] = &entry{portIdx: port, seenAt: now}
	}
	f.mu.Unlock()
}

// Lookup returns the egress port for (vid, mac). Entries older than the ageing
// duration are treated as misses (lazy expiry); gcLoop performs real deletion.
func (f *FDB) Lookup(vid uint16, mac [6]byte, now int64) (int, bool) {
	f.mu.RLock()
	e, ok := f.table[macKey{vid, mac}]
	f.mu.RUnlock()
	if !ok {
		return 0, false
	}
	if age := f.ageing.Load(); age > 0 && now-atomic.LoadInt64(&e.seenAt) > age {
		return 0, false
	}
	return e.portIdx, true
}

// reap deletes entries older than the ageing duration. No-op when ageing is 0.
func (f *FDB) reap(now int64) {
	age := f.ageing.Load()
	if age == 0 {
		return
	}
	f.mu.Lock()
	for k, e := range f.table {
		if now-atomic.LoadInt64(&e.seenAt) > age {
			delete(f.table, k)
		}
	}
	f.mu.Unlock()
}
