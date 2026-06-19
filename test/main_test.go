// Package test holds the behavioral test suite for a switch under test.
//
// The same tests run against any implementation of harness.Switch, selected by
// the SWITCH env var:
//
//	SWITCH=bridge go test ./test -v   # Linux bridge reference (default)
//
// They require root (network namespaces, veth, AF_PACKET).
package test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"vibe-switch/internal/harness"
)

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("SKIP: vibe-switch harness tests require root (netns/veth/AF_PACKET)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// settle is how long we wait after injecting before reading captures. The
// capture ring's block timeout is ~10ms, so this is comfortably enough.
const settle = 200 * time.Millisecond

// newTopology builds and starts a topology with the selected switch, wiring
// teardown into t.Cleanup.
func newTopology(t *testing.T, specs []harness.HostSpec) *harness.Topology {
	t.Helper()
	top := harness.NewTopology(t, harness.NewSwitchUnderTest(), specs)
	top.Setup()
	t.Cleanup(top.Teardown)
	return top
}

// plainHosts returns n specs with sequential IPs 10.0.0.1.. and no VLAN config.
func plainHosts(n int) []harness.HostSpec {
	specs := make([]harness.HostSpec, n)
	for i := range specs {
		specs[i] = harness.HostSpec{IP: fmt.Sprintf("10.0.0.%d", i+1)}
	}
	return specs
}
