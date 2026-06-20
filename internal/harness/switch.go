package harness

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"vibe-switch/internal/goswitch"
)

// Port is what the harness hands the switch under test: a switch-side interface
// name plus its intended VLAN role. This is the ENTIRE contract — the switch is
// free to forward however it likes (kernel bridge, AF_PACKET, XDP, ...).
type Port struct {
	SwIf      string   // switch-side interface name (in the switch netns)
	AccessVID uint16   // access port PVID (untagged egress). 0 = not access.
	Trunk     []uint16 // trunk allowed VIDs (tagged). non-empty = trunk port.
}

func (p Port) vlanAware() bool { return p.AccessVID != 0 || len(p.Trunk) > 0 }

// Switch is the test-side adapter for a switch under test. It only expresses
// "take these ports → run / stop"; it is NOT the switch's own API and may
// change freely without touching test bodies.
type Switch interface {
	Start(swNS string, ports []Port) error
	Stop() error
	Name() string
}

// AgeingConfigurable is an optional capability: switches that can tune their
// MAC-aging timeout implement it so the aging test can force a fast expiry.
// Tests skip the aging assertion for switches that do not.
type AgeingConfigurable interface {
	SetAgeing(d time.Duration) error
}

// NewSwitchUnderTest returns the implementation selected by the SWITCH env var.
// Empty or "bridge" → the Linux bridge reference.
func NewSwitchUnderTest() Switch {
	switch s := os.Getenv("SWITCH"); s {
	case "", "bridge":
		return &BridgeSwitch{}
	case "goswitch":
		return &GoSwitch{}
	default:
		panic(fmt.Sprintf("unknown SWITCH=%q (known: bridge, goswitch)", s))
	}
}

// GoSwitch adapts the user-space goswitch.Engine to the harness Switch contract.
// It opens an AF_PACKET handle inside the switch netns for each port (reusing the
// shared goswitch rx options + inbound BPF) and hands them to the engine.
type GoSwitch struct {
	eng *goswitch.Engine
}

func (g *GoSwitch) Name() string { return "go-switch" }

func (g *GoSwitch) Start(swNS string, ports []Port) error {
	cfg := make([]goswitch.PortConfig, 0, len(ports))
	ios := make([]goswitch.PacketIO, 0, len(ports))
	cleanup := func() {
		for _, io := range ios {
			io.Close()
		}
	}
	for _, p := range ports {
		tp, err := openInNetns(swNS, goswitch.RxOpts(p.SwIf)...)
		if err != nil {
			cleanup()
			return fmt.Errorf("goswitch: open %s: %w", p.SwIf, err)
		}
		if err := tp.SetBPF(goswitch.InboundBPF()); err != nil {
			tp.Close()
			cleanup()
			return fmt.Errorf("goswitch: setbpf %s: %w", p.SwIf, err)
		}
		ios = append(ios, tp)
		cfg = append(cfg, goswitch.PortConfig{Name: p.SwIf, AccessVID: p.AccessVID, Trunk: p.Trunk})
	}
	eng, err := goswitch.NewEngine(cfg, ios)
	if err != nil {
		cleanup()
		return err
	}
	g.eng = eng
	return g.eng.Start()
}

func (g *GoSwitch) Stop() error {
	if g.eng == nil {
		return nil
	}
	return g.eng.Stop()
}

// SetAgeing satisfies AgeingConfigurable so the ageing test case runs.
func (g *GoSwitch) SetAgeing(d time.Duration) error {
	if g.eng == nil {
		return fmt.Errorf("goswitch: not started")
	}
	return g.eng.SetAgeing(d)
}

// BridgeSwitch is the reference switch: a Linux kernel bridge configured with
// `ip`/`bridge` inside the switch netns.
type BridgeSwitch struct {
	swNS   string
	bridge string
}

func (b *BridgeSwitch) Name() string { return "linux-bridge" }

func (b *BridgeSwitch) Start(swNS string, ports []Port) error {
	b.swNS = swNS
	b.bridge = "br0"

	vlanAware := false
	for _, p := range ports {
		if p.vlanAware() {
			vlanAware = true
		}
	}

	// Create the bridge. STP stays off by default → ports forward immediately.
	add := []string{"link", "add", b.bridge, "type", "bridge"}
	if vlanAware {
		// vlan_default_pvid 0 prevents the kernel auto-adding VLAN 1 to ports.
		add = append(add, "vlan_filtering", "1", "vlan_default_pvid", "0")
	}
	if err := b.ns("ip", add...); err != nil {
		return err
	}
	if err := b.ns("ip", "link", "set", b.bridge, "up"); err != nil {
		return err
	}

	for _, p := range ports {
		if err := b.ns("ip", "link", "set", p.SwIf, "master", b.bridge); err != nil {
			return err
		}
		if err := b.ns("ip", "link", "set", p.SwIf, "up"); err != nil {
			return err
		}
		if !vlanAware {
			continue
		}
		switch {
		case len(p.Trunk) > 0:
			for _, vid := range p.Trunk {
				if err := b.ns("bridge", "vlan", "add", "dev", p.SwIf, "vid", strconv.Itoa(int(vid))); err != nil {
					return err
				}
			}
		case p.AccessVID != 0:
			if err := b.ns("bridge", "vlan", "add", "dev", p.SwIf,
				"vid", strconv.Itoa(int(p.AccessVID)), "pvid", "untagged"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *BridgeSwitch) Stop() error {
	if b.swNS == "" {
		return nil
	}
	// Best-effort; the netns teardown also removes the bridge.
	_ = b.ns("ip", "link", "del", b.bridge)
	return nil
}

// SetAgeing sets the bridge FDB aging time. iproute2 expresses ageing_time in
// centiseconds (default 30000 = 300s).
func (b *BridgeSwitch) SetAgeing(d time.Duration) error {
	centisec := int64(d / (10 * time.Millisecond))
	if centisec < 1 {
		centisec = 1
	}
	return b.ns("ip", "link", "set", b.bridge, "type", "bridge",
		"ageing_time", strconv.FormatInt(centisec, 10))
}

func (b *BridgeSwitch) ns(name string, args ...string) error {
	full := append([]string{"netns", "exec", b.swNS, name}, args...)
	return run("ip", full...)
}
