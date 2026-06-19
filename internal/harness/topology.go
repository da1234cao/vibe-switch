// Package harness is the test台架 for the vibe-switch project.
//
// It stands up a network topology of namespaces + veth pairs (hosts wired to
// switch ports), plugs in a "switch under test" (the Linux bridge reference
// today, a Go implementation later), and provides raw-frame inject/capture so
// behavioral tests assert purely observable switching behavior.
//
// The whole package only constrains the switch's EXTERNAL behavior; it never
// inspects implementation internals (FDB dumps, logs, ...). See doc/TDD.md.
package harness

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Broadcast is the all-ones destination MAC.
var Broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

var topoSeq int64

// HostSpec describes one port/host of a topology.
type HostSpec struct {
	IP        string   // optional IPv4 (no mask); assigned as /24. "" = no IP.
	AccessVID uint16   // if non-zero (and Trunk empty): access port with this PVID, untagged.
	Trunk     []uint16 // if non-empty: trunk port carrying these VIDs tagged.
}

// Host is one end-host attached to a switch port.
type Host struct {
	Idx       int
	NS        string // host network namespace name
	IfName    string // interface name inside the host netns (always "eth0")
	MAC       net.HardwareAddr
	IP        string
	SwIf      string // switch-side veth peer name (lives in SwNS)
	AccessVID uint16
	Trunk     []uint16
}

// Topology owns the namespaces/links and the switch under test.
type Topology struct {
	prefix string
	SwNS   string // switch-side network namespace (holds the switch + swX ports)
	Hosts  []*Host
	sw     Switch
	t      *testing.T
}

// NewTopology builds (but does not realize) a topology description. Call Setup
// to create it on the system.
func NewTopology(t *testing.T, sw Switch, specs []HostSpec) *Topology {
	seq := atomic.AddInt64(&topoSeq, 1)
	prefix := fmt.Sprintf("vs%dx%d", os.Getpid()%1000, seq)
	top := &Topology{prefix: prefix, SwNS: prefix + "sw", sw: sw, t: t}
	for i, s := range specs {
		idx := i + 1
		top.Hosts = append(top.Hosts, &Host{
			Idx:       idx,
			NS:        fmt.Sprintf("%sh%d", prefix, idx),
			IfName:    "eth0",
			MAC:       net.HardwareAddr{0x02, 0, 0, 0, 0, byte(idx)},
			IP:        s.IP,
			SwIf:      fmt.Sprintf("%ss%d", prefix, idx),
			AccessVID: s.AccessVID,
			Trunk:     s.Trunk,
		})
	}
	return top
}

// Setup realizes the topology: creates namespaces, veth pairs, configures the
// host side (MAC/IP/up/promisc, IPv6 disabled), then starts the switch.
func (top *Topology) Setup() {
	t := top.t
	top.mustIP("netns", "add", top.SwNS)

	ports := make([]Port, 0, len(top.Hosts))
	for _, h := range top.Hosts {
		top.mustIP("netns", "add", h.NS)

		hostTmp := top.prefix + "p" + strconv.Itoa(h.Idx) // temp name before rename
		top.mustIP("link", "add", h.SwIf, "type", "veth", "peer", "name", hostTmp)
		top.mustIP("link", "set", h.SwIf, "netns", top.SwNS)
		top.mustIP("link", "set", hostTmp, "netns", h.NS)

		// Host side.
		top.mustNS(h.NS, "ip", "link", "set", hostTmp, "name", h.IfName)
		top.mustNS(h.NS, "ip", "link", "set", h.IfName, "address", h.MAC.String())
		top.mustNS(h.NS, "sysctl", "-q", "-w", "net.ipv6.conf."+h.IfName+".disable_ipv6=1")
		if h.IP != "" {
			top.mustNS(h.NS, "ip", "addr", "add", h.IP+"/24", "dev", h.IfName)
		}
		top.mustNS(h.NS, "ip", "link", "set", h.IfName, "up")
		// Promiscuous so AF_PACKET reliably captures flooded frames addressed
		// to foreign/unknown MACs.
		top.mustNS(h.NS, "ip", "link", "set", h.IfName, "promisc", "on")

		// Switch side: silence IPv6 noise, bring up; the switch enslaves/owns it.
		top.mustNS(top.SwNS, "sysctl", "-q", "-w", "net.ipv6.conf."+h.SwIf+".disable_ipv6=1")
		top.mustNS(top.SwNS, "ip", "link", "set", h.SwIf, "up")

		ports = append(ports, Port{SwIf: h.SwIf, AccessVID: h.AccessVID, Trunk: h.Trunk})
	}

	if err := top.sw.Start(top.SwNS, ports); err != nil {
		t.Fatalf("switch %q start: %v", top.sw.Name(), err)
	}
}

// Teardown removes everything. Safe to call on partially-built topologies and
// always runs (wire it via t.Cleanup).
func (top *Topology) Teardown() {
	if top.sw != nil {
		_ = top.sw.Stop()
	}
	for _, h := range top.Hosts {
		_ = run("ip", "netns", "del", h.NS) // deletes interfaces inside too
	}
	_ = run("ip", "netns", "del", top.SwNS)
}

// Switch returns the switch under test (e.g. to access optional capabilities
// such as AgeingConfigurable).
func (top *Topology) Switch() Switch { return top.sw }

// Host returns the host with 1-based index idx.
func (top *Topology) Host(idx int) *Host {
	for _, h := range top.Hosts {
		if h.Idx == idx {
			return h
		}
	}
	top.t.Fatalf("no host with index %d", idx)
	return nil
}

// MAC is the hardware address of host idx.
func (top *Topology) MAC(idx int) net.HardwareAddr { return top.Host(idx).MAC }

// Ping runs a single ping from host fromIdx to toIP, returning an error if it
// does not get a reply within 1s.
func (top *Topology) Ping(fromIdx int, toIP string) error {
	h := top.Host(fromIdx)
	return run("ip", "netns", "exec", h.NS, "ping", "-c", "1", "-W", "1", toIP)
}

// --- shell helpers ---

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (top *Topology) mustIP(args ...string) {
	if err := run("ip", args...); err != nil {
		top.t.Fatalf("setup: %v", err)
	}
}

// mustNS runs `ip netns exec <ns> <name> <args...>`.
func (top *Topology) mustNS(ns, name string, args ...string) {
	full := append([]string{"netns", "exec", ns, name}, args...)
	if err := run("ip", full...); err != nil {
		top.t.Fatalf("setup: %v", err)
	}
}
