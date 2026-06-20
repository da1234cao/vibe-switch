// Package control is the runtime management interface for a running vibe-switch:
// a standard-library net/rpc server over a Unix socket exposing read-only engine
// snapshots, plus a matching client used by the `vibe-switch ctl` subcommand.
package control

import (
	"net"
	"net/rpc"
	"os"
	"sync"

	"vibe-switch/internal/goswitch"
)

// Empty is the argument type for no-input RPC methods (net/rpc requires a named,
// exported, gob-encodable arg).
type Empty struct{}

// rpcName is the registered service name; clients call "Switch.<Method>".
const rpcName = "Switch"

// Control is the RPC receiver. Methods return engine snapshots verbatim.
type Control struct {
	eng *goswitch.Engine
}

func (c *Control) FDB(_ Empty, reply *[]goswitch.FDBEntry) error {
	*reply = c.eng.FDBSnapshot()
	return nil
}

func (c *Control) Ports(_ Empty, reply *[]goswitch.PortInfo) error {
	*reply = c.eng.PortsSnapshot()
	return nil
}

func (c *Control) Stats(_ Empty, reply *[]goswitch.PortStats) error {
	*reply = c.eng.StatsSnapshot()
	return nil
}

func (c *Control) Config(_ Empty, reply *goswitch.EngineConfig) error {
	*reply = c.eng.ConfigSnapshot()
	return nil
}

// Server is a running control endpoint.
type Server struct {
	ln       net.Listener
	sockPath string
	srv      *rpc.Server
	done     chan struct{}
	wg       sync.WaitGroup
}

// Serve registers eng's snapshots and listens on sockPath. A stale socket file
// from a previous crash is unlinked first (otherwise Listen fails with "address
// already in use").
func Serve(eng *goswitch.Engine, sockPath string) (*Server, error) {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	srv := rpc.NewServer()
	if err := srv.RegisterName(rpcName, &Control{eng: eng}); err != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, err
	}
	s := &Server{ln: ln, sockPath: sockPath, srv: srv, done: make(chan struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed on shutdown, or fatal
		}
		go s.srv.ServeConn(conn)
	}
}

// Close stops accepting, waits for the accept loop, and removes the socket file.
func (s *Server) Close() error {
	close(s.done)
	err := s.ln.Close() // unblocks Accept
	s.wg.Wait()
	os.Remove(s.sockPath)
	return err
}
