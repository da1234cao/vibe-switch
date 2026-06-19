package test

import (
	"net"
	"testing"
	"time"

	"vibe-switch/internal/harness"
)

// 1. Connectivity: two hosts on the switch can ping each other.
func TestL2Connectivity(t *testing.T) {
	top := newTopology(t, plainHosts(2))
	if err := top.Ping(1, "10.0.0.2"); err != nil {
		t.Fatalf("h1 → h2 ping should succeed: %v", err)
	}
}

// 2. Known unicast is forwarded to exactly the learned port, not flooded.
func TestL2KnownUnicastNotFlooded(t *testing.T) {
	top := newTopology(t, plainHosts(4))

	// Teach the switch where h2 lives.
	top.Inject(2, harness.BuildFrame(top.MAC(2), harness.Broadcast, []byte("learn"), 0))
	time.Sleep(100 * time.Millisecond)

	caps := top.Capture(2, 3, 4)
	defer caps.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), top.MAC(2), []byte("hi"), 0))
	time.Sleep(settle)

	if caps.Count(2) == 0 {
		t.Errorf("h2 (the destination) should receive the unicast frame")
	}
	if n := caps.Count(3); n != 0 {
		t.Errorf("h3 must not receive known unicast, got %d frame(s)", n)
	}
	if n := caps.Count(4); n != 0 {
		t.Errorf("h4 must not receive known unicast, got %d frame(s)", n)
	}
}

// 3. Unknown unicast (unlearned destination) is flooded to all other ports.
func TestL2UnknownUnicastFlooded(t *testing.T) {
	top := newTopology(t, plainHosts(4))

	caps := top.Capture(2, 3, 4)
	defer caps.Close()
	unknown := net.HardwareAddr{0x02, 0, 0, 0, 0, 0xFE} // never learned
	top.Inject(1, harness.BuildFrame(top.MAC(1), unknown, []byte("flood"), 0))
	time.Sleep(settle)

	for _, idx := range []int{2, 3, 4} {
		if caps.Count(idx) == 0 {
			t.Errorf("h%d should receive flooded unknown unicast", idx)
		}
	}
}

// 4. Broadcast is flooded to all other ports.
func TestL2BroadcastFlooded(t *testing.T) {
	top := newTopology(t, plainHosts(4))

	caps := top.Capture(2, 3, 4)
	defer caps.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), harness.Broadcast, []byte("bcast"), 0))
	time.Sleep(settle)

	for _, idx := range []int{2, 3, 4} {
		if caps.Count(idx) == 0 {
			t.Errorf("h%d should receive broadcast", idx)
		}
	}
}

// 5. A frame is never sent back out its ingress port.
func TestL2NoReflection(t *testing.T) {
	top := newTopology(t, plainHosts(3))

	caps := top.Capture(1, 2) // also watch the sender's own port
	defer caps.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), harness.Broadcast, []byte("refl"), 0))
	time.Sleep(settle)

	if n := caps.Count(1); n != 0 {
		t.Errorf("frame must not come back out the ingress port, got %d", n)
	}
	if caps.Count(2) == 0 {
		t.Errorf("sanity: broadcast should still reach h2")
	}
}

// 6. A learned entry ages out: after the aging timeout, traffic to it floods
// again. Skipped for switches that cannot tune their aging time.
func TestL2MACAging(t *testing.T) {
	top := newTopology(t, plainHosts(3))
	ageable, ok := top.Switch().(harness.AgeingConfigurable)
	if !ok {
		t.Skipf("switch %q is not ageing-configurable", top.Switch().Name())
	}
	const age = 1 * time.Second
	if err := ageable.SetAgeing(age); err != nil {
		t.Fatalf("set ageing: %v", err)
	}

	// Learn h2, then confirm unicast to it is NOT flooded to h3.
	top.Inject(2, harness.BuildFrame(top.MAC(2), harness.Broadcast, []byte("learn"), 0))
	time.Sleep(100 * time.Millisecond)
	caps := top.Capture(3)
	top.Inject(1, harness.BuildFrame(top.MAC(1), top.MAC(2), []byte("known"), 0))
	time.Sleep(settle)
	if n := caps.Count(3); n != 0 {
		t.Errorf("precondition: known unicast should not reach h3, got %d", n)
	}
	caps.Close()

	// Let the entry age out (timeout + GC sweep margin), with no traffic to
	// refresh it (IPv6 is disabled on all interfaces).
	time.Sleep(age + 2500*time.Millisecond)

	caps2 := top.Capture(3)
	defer caps2.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), top.MAC(2), []byte("aged"), 0))
	time.Sleep(settle)
	if caps2.Count(3) == 0 {
		t.Errorf("after aging, unicast to the expired entry should re-flood to h3")
	}
}
