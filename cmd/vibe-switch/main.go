// Command vibe-switch runs the user-space L2/VLAN switch on real host
// interfaces, or (via the `ctl` subcommand) queries a running instance.
//
//	vibe-switch -i eth0 -i eth1                 # plain L2, one broadcast domain
//	vibe-switch -access eth0:10 -trunk eth1:10,20
//	vibe-switch ctl show fdb                    # query a running switch
//	vibe-switch ctl                             # interactive shell
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vibe-switch/internal/control"
	"vibe-switch/internal/goswitch"
)

const defaultSock = "/run/vibe-switch.sock"

func main() {
	// Subcommand dispatch: `ctl` is the RPC client; anything else runs a switch.
	if len(os.Args) >= 2 && os.Args[1] == "ctl" {
		if err := runCtl(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "vibe-switch ctl:", err)
			os.Exit(1)
		}
		return
	}
	if err := runSwitch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vibe-switch:", err)
		os.Exit(1)
	}
}

// portCollector is a repeatable flag that appends to a shared, order-preserving
// config slice. The same target slice backs -i/-access/-trunk, so ports keep
// their command-line order regardless of which flag introduced them.
type portCollector struct {
	cfgs *[]goswitch.PortConfig
	role string // "plain" | "access" | "trunk"
}

func (c portCollector) String() string { return "" }

func (c portCollector) Set(v string) error {
	switch c.role {
	case "plain":
		*c.cfgs = append(*c.cfgs, goswitch.PortConfig{Name: v})
	case "access":
		name, vid, err := splitNameVID(v)
		if err != nil {
			return err
		}
		*c.cfgs = append(*c.cfgs, goswitch.PortConfig{Name: name, AccessVID: vid})
	case "trunk":
		name, vids, err := splitNameVIDs(v)
		if err != nil {
			return err
		}
		*c.cfgs = append(*c.cfgs, goswitch.PortConfig{Name: name, Trunk: vids})
	}
	return nil
}

func splitNameVID(s string) (string, uint16, error) {
	name, rest, ok := strings.Cut(s, ":")
	if !ok || name == "" {
		return "", 0, fmt.Errorf("expected iface:vid, got %q", s)
	}
	vid, err := parseVID(rest)
	return name, vid, err
}

func splitNameVIDs(s string) (string, []uint16, error) {
	name, rest, ok := strings.Cut(s, ":")
	if !ok || name == "" {
		return "", nil, fmt.Errorf("expected iface:vid[,vid...], got %q", s)
	}
	var vids []uint16
	for _, part := range strings.Split(rest, ",") {
		vid, err := parseVID(part)
		if err != nil {
			return "", nil, err
		}
		vids = append(vids, vid)
	}
	if len(vids) == 0 {
		return "", nil, fmt.Errorf("trunk %q: no VIDs", s)
	}
	return name, vids, nil
}

func parseVID(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 || n > 4094 {
		return 0, fmt.Errorf("invalid VID %q (1-4094)", s)
	}
	return uint16(n), nil
}

func runSwitch(args []string) error {
	fs := flag.NewFlagSet("vibe-switch", flag.ContinueOnError)
	var cfgs []goswitch.PortConfig
	fs.Var(portCollector{&cfgs, "plain"}, "i", "plain L2 port (repeatable): -i eth0")
	fs.Var(portCollector{&cfgs, "access"}, "access", "access port (repeatable): -access eth0:10")
	fs.Var(portCollector{&cfgs, "trunk"}, "trunk", "trunk port (repeatable): -trunk eth1:10,20")
	ageing := fs.Int("ageing", 300, "MAC ageing time in seconds (0 = never)")
	sock := fs.String("ctl-sock", defaultSock, "control socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(cfgs) == 0 {
		fs.Usage()
		return errors.New("no ports; specify at least one -i/-access/-trunk")
	}

	// Open an AF_PACKET handle per port up front; clean up on any failure.
	ios := make([]goswitch.PacketIO, 0, len(cfgs))
	for _, c := range cfgs {
		tp, err := goswitch.OpenInterface(c.Name)
		if err != nil {
			for _, io := range ios {
				io.Close()
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("open %s: %w (AF_PACKET needs root)", c.Name, err)
			}
			return fmt.Errorf("open %s: %w", c.Name, err)
		}
		ios = append(ios, tp)
	}

	eng, err := goswitch.NewEngine(cfgs, ios)
	if err != nil {
		for _, io := range ios {
			io.Close()
		}
		return err
	}
	if err := eng.SetAgeing(time.Duration(*ageing) * time.Second); err != nil {
		return err
	}
	if err := eng.Start(); err != nil {
		return err
	}

	srv, err := control.Serve(eng, *sock)
	if err != nil {
		eng.Stop()
		return fmt.Errorf("control socket: %w", err)
	}

	for _, p := range eng.PortsSnapshot() {
		fmt.Printf("port %s role=%s\n", p.Name, p.Role)
	}
	fmt.Printf("vibe-switch up: %d ports, ctl socket %s (Ctrl-C to stop)\n", len(cfgs), *sock)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Fprintln(os.Stderr, "\nshutting down…")
	srv.Close()
	return eng.Stop()
}

func runCtl(args []string) error {
	fs := flag.NewFlagSet("vibe-switch ctl", flag.ContinueOnError)
	sock := fs.String("ctl-sock", defaultSock, "control socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return control.Shell(*sock) // interactive
	}
	if rest[0] != "show" || len(rest) < 2 {
		return fmt.Errorf("usage: vibe-switch ctl [show fdb|ports|stats|config|all]")
	}
	return control.Show(*sock, rest[1])
}
