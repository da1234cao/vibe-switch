package goswitch

import (
	"net"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

type portRole int

const (
	rolePlain  portRole = iota // transparent L2, no VLAN awareness (internal vid 0)
	roleAccess                 // untagged member of one VLAN (pvid)
	roleTrunk                  // tagged member of a set of VLANs
)

func (r portRole) String() string {
	switch r {
	case roleAccess:
		return "access"
	case roleTrunk:
		return "trunk"
	default:
		return "plain"
	}
}

// roleOf derives a port's role from its harness/CLI config.
func roleOf(accessVID uint16, trunk []uint16) portRole {
	switch {
	case len(trunk) > 0:
		return roleTrunk
	case accessVID != 0:
		return roleAccess
	default:
		return rolePlain
	}
}

// classify maps an ingress frame to its internal VLAN id based on the ingress
// port's role. ok=false means "drop" (illegal tag for the role).
func (p *port) classify(hasTag bool, tagVID uint16) (vid uint16, ok bool) {
	switch p.role {
	case rolePlain:
		return 0, true // single broadcast domain
	case roleAccess:
		if hasTag {
			return 0, false // access ports don't accept tagged frames
		}
		return p.pvid, true
	case roleTrunk:
		if !hasTag {
			return 0, false // no native VLAN
		}
		if !p.allow[tagVID] {
			return 0, false
		}
		return tagVID, true
	}
	return 0, false
}

// serializeOpts: FixLengths keeps any encapsulated L3 length fields consistent;
// we never recompute checksums (raw passthrough of opaque payload).
var serializeOpts = gopacket.SerializeOptions{FixLengths: true}

// buildTagged re-serializes a frame as 802.1Q-tagged on egress (access/plain →
// trunk). innerType/payload are the post-Ethernet contents of the ingress frame.
func buildTagged(dst, src net.HardwareAddr, vid uint16, innerType layers.EthernetType, payload []byte) []byte {
	eth := &layers.Ethernet{SrcMAC: src, DstMAC: dst, EthernetType: layers.EthernetTypeDot1Q}
	dot1q := &layers.Dot1Q{VLANIdentifier: vid, Type: innerType}
	buf := gopacket.NewSerializeBuffer()
	_ = gopacket.SerializeLayers(buf, serializeOpts, eth, dot1q, gopacket.Payload(payload))
	return buf.Bytes()
}

// buildUntagged re-serializes a frame without a tag on egress (trunk → access).
// innerType/payload are the post-tag contents of the ingress frame.
func buildUntagged(dst, src net.HardwareAddr, innerType layers.EthernetType, payload []byte) []byte {
	eth := &layers.Ethernet{SrcMAC: src, DstMAC: dst, EthernetType: innerType}
	buf := gopacket.NewSerializeBuffer()
	_ = gopacket.SerializeLayers(buf, serializeOpts, eth, gopacket.Payload(payload))
	return buf.Bytes()
}
