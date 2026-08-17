package client

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
)

// Pins the rule either side of every range it turns on.
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
		// Carrier grade NAT, which mobile networks hand out.
		{"/ip4/100.64.0.1/tcp/4001", false},
		{"/ip4/169.254.1.1/tcp/4001", false},

		// Loopback.
		{"/ip4/127.0.0.1/tcp/4001", true},
		{"/ip6/::1/tcp/4001", true},

		// Just outside the private ranges.
		{"/ip4/172.15.0.16/tcp/4001", true},
		{"/ip4/172.32.0.1/tcp/4001", true},
	} {
		if got := gater.InterceptAddrDial(discovered, ma.StringCast(test.addr)); got != test.want {
			t.Errorf("InterceptAddrDial(%s) = %v, want %v", test.addr, got, test.want)
		}
	}
}

// Pins the exemption, and that it does not extend to anyone else.
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

// Pins that only the address check gates anything.
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
