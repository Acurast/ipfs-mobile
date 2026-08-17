package client

import (
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// discoveredAddrGater keeps dials that cannot succeed off the wire. A peer found
// through the DHT is dialled on every address it advertises, and nodes announce
// the addresses they see themselves on - container and LAN ones included, which
// name a network this device is not on. go-libp2p shortens the timeout for those
// rather than skipping them, so each one costs a handshake that cannot complete
// and, until it times out, an entry in every NAT table on the way out. That is
// what a farm of devices behind one router exhausts first.
//
// The rule is that a private address has to be named in the configuration to be
// dialled: reaching a peer on the local network is a deliberate choice, not
// something to infer from what a stranger advertises. Loopback stays dialable
// since it never leaves the device, and so costs nothing this is here to save.
// Addresses that are not private - relay and DNS ones among them - are left to
// libp2p. Only dialling is gated; what reaches this device already arrived.
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

	if !manet.IsPrivateAddr(addr) {
		return true
	}

	return manet.IsIPLoopback(addr)
}

// A peer is only ever reached through an address, so the address is where the
// decision belongs; the rest of the interface allows everything.
func (discoveredAddrGater) InterceptPeerDial(peer.ID) bool { return true }

func (discoveredAddrGater) InterceptAccept(network.ConnMultiaddrs) bool { return true }

func (discoveredAddrGater) InterceptSecured(network.Direction, peer.ID, network.ConnMultiaddrs) bool {
	return true
}

func (discoveredAddrGater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}
