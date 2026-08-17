package client

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
)

// The addresses a stranger advertises decide nothing about the local network, so
// the private ones among them are not dialled - while a public address, or one
// this device can reach without leaving itself, still is.
func TestGaterRefusesPrivateAddressesOfDiscoveredPeers(t *testing.T) {
	gater := newDiscoveredAddrGater(nil)

	const discovered = peer.ID("discovered")

	for _, test := range []struct {
		addr string
		want bool
	}{
		{"/ip4/104.131.131.82/tcp/4001", true},
		{"/ip6/2606:4700:4700::1111/tcp/4001", true},
		{"/dns4/example.com/tcp/443/wss", true},

		{"/ip4/10.0.0.1/tcp/4001", false},
		{"/ip4/172.17.0.1/tcp/4001", false},
		{"/ip4/172.31.255.254/tcp/4001", false},
		{"/ip4/192.168.1.5/tcp/4001", false},
		// Carrier grade NAT: a device behind it is no more reachable than one on a
		// LAN, and mobile networks hand these out.
		{"/ip4/100.64.0.1/tcp/4001", false},
		{"/ip4/169.254.1.1/tcp/4001", false},

		// Never leaves the device, so it costs nothing this gater exists to save.
		{"/ip4/127.0.0.1/tcp/4001", true},
		{"/ip6/::1/tcp/4001", true},

		// Just outside the private ranges, so still worth dialling.
		{"/ip4/172.15.0.16/tcp/4001", true},
		{"/ip4/172.32.0.1/tcp/4001", true},
	} {
		if got := gater.InterceptAddrDial(discovered, ma.StringCast(test.addr)); got != test.want {
			t.Errorf("InterceptAddrDial(%s) = %v, want %v", test.addr, got, test.want)
		}
	}
}

// A peer on the local network is reached by naming it, which is the case the
// filter above must not take away.
func TestGaterDialsPrivateAddressesOfConfiguredPeers(t *testing.T) {
	const configured = peer.ID("configured")

	gater := newDiscoveredAddrGater([]peer.AddrInfo{{ID: configured}})

	for _, addr := range []string{
		"/ip4/192.168.1.5/tcp/4001",
		"/ip4/10.0.0.1/tcp/4001",
	} {
		if !gater.InterceptAddrDial(configured, ma.StringCast(addr)) {
			t.Errorf("a configured peer was refused %s", addr)
		}
		if gater.InterceptAddrDial(peer.ID("someone-else"), ma.StringCast(addr)) {
			t.Errorf("%s was dialled for a peer that is not configured", addr)
		}
	}
}

// Inbound connections and the peer level checks are not what costs the dials, so
// they stay open.
func TestGaterGatesOnlyDialledAddresses(t *testing.T) {
	gater := newDiscoveredAddrGater(nil)

	if !gater.InterceptPeerDial(peer.ID("anyone")) {
		t.Error("a peer was refused before any address was considered")
	}
	if !gater.InterceptAccept(nil) {
		t.Error("an inbound connection was refused")
	}
	if !gater.InterceptSecured(0, peer.ID("anyone"), nil) {
		t.Error("a secured connection was refused")
	}
	if allow, _ := gater.InterceptUpgraded(nil); !allow {
		t.Error("an upgraded connection was refused")
	}
}
