package client

import (
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// discoveredAddrGater keeps dials that cannot succeed off the wire. Peers found
// through the DHT advertise the addresses they see themselves on, container and
// LAN ones included, which name a network this device is not on. Nothing else
// filters them, so each costs a handshake that cannot complete and, until it
// times out, an entry in every NAT table on the way out.
//
// A private address therefore has to be named in the configuration to be dialled:
// reaching a peer on the local network is a deliberate choice, not something to
// infer from what a stranger advertises. Loopback is exempt because it never
// leaves the device. Only dialling is gated; what reaches this device already
// arrived.
type discoveredAddrGater struct {
	configured map[peer.ID]struct{}
}

var _ connmgr.ConnectionGater = discoveredAddrGater{}

func newDiscoveredAddrGater(configured []peer.AddrInfo) discoveredAddrGater {
	exempt := make(map[peer.ID]struct{}, len(configured))
	for _, peerInfo := range configured {
		exempt[peerInfo.ID] = struct{}{}
	}

	return discoveredAddrGater{configured: exempt}
}

func (gater discoveredAddrGater) InterceptAddrDial(id peer.ID, addr ma.Multiaddr) bool {
	if _, configured := gater.configured[id]; configured {
		return true
	}

	return dialableAddr(addr)
}

// dialableAddr applies the rule discoveredAddrGater documents to one address.
func dialableAddr(addr ma.Multiaddr) bool {
	if !manet.IsPrivateAddr(addr) {
		return true
	}

	return manet.IsIPLoopback(addr)
}

// A peer is only ever reached through an address, so that is where the decision
// belongs.
func (discoveredAddrGater) InterceptPeerDial(peer.ID) bool { return true }

func (discoveredAddrGater) InterceptAccept(network.ConnMultiaddrs) bool { return true }

func (discoveredAddrGater) InterceptSecured(network.Direction, peer.ID, network.ConnMultiaddrs) bool {
	return true
}

func (discoveredAddrGater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}
