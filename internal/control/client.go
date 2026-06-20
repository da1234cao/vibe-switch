package control

import (
	"bufio"
	"fmt"
	"io"
	"net/rpc"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"vibe-switch/internal/goswitch"
)

// Show dials the server, runs one `show <what>` query, and prints it.
func Show(sockPath, what string) error {
	client, err := rpc.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("connect %s: %w", sockPath, err)
	}
	defer client.Close()
	return query(client, what, os.Stdout)
}

// Shell runs an interactive REPL: each `show ...` line is one RPC on a reused
// connection; state always comes fresh from the server.
func Shell(sockPath string) error {
	client, err := rpc.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("connect %s: %w", sockPath, err)
	}
	defer client.Close()

	fmt.Fprintln(os.Stderr, "vibe-switch ctl — commands: show fdb|ports|stats|config|all, help, quit")
	sc := bufio.NewScanner(os.Stdin)
	fmt.Fprint(os.Stderr, "vibe-switch> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case line == "quit" || line == "exit":
			return nil
		case line == "help":
			fmt.Fprintln(os.Stderr, "show fdb|ports|stats|config|all | quit")
		case strings.HasPrefix(line, "show "):
			if err := query(client, strings.TrimSpace(line[len("show "):]), os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		default:
			fmt.Fprintln(os.Stderr, "unknown command:", line, "(try: help)")
		}
		fmt.Fprint(os.Stderr, "vibe-switch> ")
	}
	fmt.Fprintln(os.Stderr)
	return sc.Err()
}

// query performs one snapshot RPC and renders it to w.
func query(client *rpc.Client, what string, w io.Writer) error {
	switch what {
	case "fdb":
		var r []goswitch.FDBEntry
		if err := client.Call(rpcName+".FDB", Empty{}, &r); err != nil {
			return err
		}
		renderFDB(w, r)
	case "ports":
		var r []goswitch.PortInfo
		if err := client.Call(rpcName+".Ports", Empty{}, &r); err != nil {
			return err
		}
		renderPorts(w, r)
	case "stats":
		var r []goswitch.PortStats
		if err := client.Call(rpcName+".Stats", Empty{}, &r); err != nil {
			return err
		}
		renderStats(w, r)
	case "config":
		var r goswitch.EngineConfig
		if err := client.Call(rpcName+".Config", Empty{}, &r); err != nil {
			return err
		}
		renderConfig(w, r)
	case "all":
		for _, sub := range []string{"config", "ports", "fdb", "stats"} {
			fmt.Fprintf(w, "== %s ==\n", sub)
			if err := query(client, sub, w); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown target %q (fdb|ports|stats|config|all)", what)
	}
	return nil
}

func renderFDB(w io.Writer, entries []goswitch.FDBEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].VID != entries[j].VID {
			return entries[i].VID < entries[j].VID
		}
		return entries[i].MAC < entries[j].MAC
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VLAN\tMAC\tPORT\tAGE(s)")
	for _, e := range entries {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%.1f\n", e.VID, e.MAC, e.Port, e.AgeSeconds)
	}
	tw.Flush()
	if len(entries) == 0 {
		fmt.Fprintln(w, "(empty)")
	}
}

func renderPorts(w io.Writer, ports []goswitch.PortInfo) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tROLE\tPVID\tTRUNK\tUP")
	for _, p := range ports {
		trunk := "-"
		if len(p.Trunk) > 0 {
			parts := make([]string, len(p.Trunk))
			for i, v := range p.Trunk {
				parts[i] = fmt.Sprintf("%d", v)
			}
			trunk = strings.Join(parts, ",")
		}
		pvid := "-"
		if p.PVID != 0 {
			pvid = fmt.Sprintf("%d", p.PVID)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\n", p.Name, p.Role, pvid, trunk, p.Up)
	}
	tw.Flush()
}

func renderStats(w io.Writer, stats []goswitch.PortStats) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tRX_FRAMES\tRX_BYTES\tTX_FRAMES\tTX_BYTES\tFLOODED\tDROPPED")
	for _, s := range stats {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n",
			s.Name, s.RxFrames, s.RxBytes, s.TxFrames, s.TxBytes, s.Flooded, s.Dropped)
	}
	tw.Flush()
}

func renderConfig(w io.Writer, c goswitch.EngineConfig) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ports\t%d\n", c.NumPorts)
	fmt.Fprintf(tw, "vlan-aware\t%t\n", c.VLANAware)
	fmt.Fprintf(tw, "ageing(s)\t%.0f\n", c.AgeingSeconds)
	tw.Flush()
}
