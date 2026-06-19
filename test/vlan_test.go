package test

import (
	"testing"
	"time"

	"vibe-switch/internal/harness"
)

// vlanHosts builds the standard VLAN topology:
//
//	h1, h2 = access VLAN 10   (IPs .1, .2)
//	h3     = access VLAN 20   (IP  .3)
//	h4, h5 = trunk (tagged 10, 20)
func vlanHosts() []harness.HostSpec {
	return []harness.HostSpec{
		{IP: "10.0.0.1", AccessVID: 10},
		{IP: "10.0.0.2", AccessVID: 10},
		{IP: "10.0.0.3", AccessVID: 20},
		{Trunk: []uint16{10, 20}},
		{Trunk: []uint16{10, 20}},
	}
}

// 7. Hosts in the same VLAN can communicate.
func TestVLANSameVLANConnectivity(t *testing.T) {
	top := newTopology(t, vlanHosts())
	if err := top.Ping(1, "10.0.0.2"); err != nil {
		t.Fatalf("same-VLAN ping h1 → h2 should succeed: %v", err)
	}
}

// 8. Hosts in different VLANs are isolated (ARP/ping cannot cross).
func TestVLANCrossVLANIsolation(t *testing.T) {
	top := newTopology(t, vlanHosts())
	if err := top.Ping(1, "10.0.0.3"); err == nil {
		t.Errorf("cross-VLAN ping h1(v10) → h3(v20) should fail")
	}
}

// 9. Access ports deliver frames untagged.
func TestVLANAccessEgressUntagged(t *testing.T) {
	top := newTopology(t, vlanHosts())

	caps := top.Capture(2)
	defer caps.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), harness.Broadcast, []byte("v10"), 0))
	time.Sleep(settle)

	got := caps.Received(2)
	if len(got) == 0 {
		t.Fatalf("h2 (same VLAN access) should receive the frame")
	}
	for _, fi := range got {
		if fi.HasVLAN {
			t.Errorf("access egress must be untagged, got VLAN %d", fi.VLANID)
		}
	}
}

// 10. Trunk passthrough: a tagged frame entering a trunk port is delivered
// tagged out the other trunk, untagged to same-VLAN access ports, and not at
// all to a different VLAN.
func TestVLANTrunkPassthrough(t *testing.T) {
	top := newTopology(t, vlanHosts())

	caps := top.Capture(1, 2, 3, 5)
	defer caps.Close()
	top.Inject(4, harness.BuildVLANFrame(top.MAC(4), harness.Broadcast, 10, []byte("trunk10"), 0))
	time.Sleep(settle)

	// h5 (other trunk): tag preserved.
	r5 := caps.Received(5)
	if len(r5) == 0 {
		t.Fatalf("h5 (trunk) should receive the VLAN 10 frame")
	}
	for _, fi := range r5 {
		if !fi.HasVLAN || fi.VLANID != 10 {
			t.Errorf("trunk egress should keep tag VLAN 10, got HasVLAN=%v VLAN=%d", fi.HasVLAN, fi.VLANID)
		}
	}

	// h1, h2 (access v10): tag stripped.
	for _, idx := range []int{1, 2} {
		r := caps.Received(idx)
		if len(r) == 0 {
			t.Errorf("h%d (access v10) should receive the frame", idx)
		}
		for _, fi := range r {
			if fi.HasVLAN {
				t.Errorf("h%d access egress must be untagged, got VLAN %d", idx, fi.VLANID)
			}
		}
	}

	// h3 (access v20): isolated.
	if n := caps.Count(3); n != 0 {
		t.Errorf("h3 (VLAN 20) must not receive VLAN 10 traffic, got %d", n)
	}
}

// 11. PVID: an untagged frame on an access port is classified to that port's
// VLAN and reaches only same-VLAN members.
func TestVLANPVIDClassification(t *testing.T) {
	top := newTopology(t, vlanHosts())

	caps := top.Capture(2, 3)
	defer caps.Close()
	top.Inject(1, harness.BuildFrame(top.MAC(1), harness.Broadcast, []byte("pvid"), 0))
	time.Sleep(settle)

	if caps.Count(2) == 0 {
		t.Errorf("h2 (same VLAN 10) should receive the untagged frame")
	}
	if n := caps.Count(3); n != 0 {
		t.Errorf("h3 (VLAN 20) must not receive it, got %d", n)
	}
}
