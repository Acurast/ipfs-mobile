package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"

	madns "github.com/multiformats/go-multiaddr-dns"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
)

const dnsPort = "53"

// newResolver builds a resolver that queries the given servers directly, for
// libp2p to resolve addresses with.
//
// A /dnsaddr/ address is a TXT record, and Android publishes no resolver
// configuration for TXT lookups to be built from, so they fail there while plain
// host lookups go through the platform and succeed. Naming the servers is what
// makes /dnsaddr/ resolvable, bootstrap peers included.
//
// No servers returns no resolver, leaving libp2p its own.
func newResolver(servers []string) (network.MultiaddrDNSResolver, error) {
	if len(servers) == 0 {
		return nil, nil
	}

	addrs := make([]string, 0, len(servers))

	for _, server := range servers {
		addr, err := dnsServerAddr(server)
		if err != nil {
			fmt.Printf("skipping invalid dns server %q: %s\n", server, err)
			continue
		}

		addrs = append(addrs, addr)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("none of the %d configured dns servers is a valid address", len(servers))
	}

	var (
		dialer net.Dialer
		next   atomic.Uint64
	)

	basic := &net.Resolver{
		// The query has to be built here, since the platform resolver is the part
		// that cannot answer it.
		PreferGo: true,

		// The address Go chose comes from configuration Android does not publish, so
		// it is discarded in favour of a server that was named. Successive lookups
		// start from successive servers so one unresponsive server is not on the
		// critical path for all of them.
		Dial: func(ctx context.Context, transport string, _ string) (net.Conn, error) {
			chosen := addrs[int(next.Add(1)-1)%len(addrs)]

			return dialer.DialContext(ctx, transport, chosen)
		},
	}

	resolver, err := madns.NewResolver(madns.WithDefaultResolver(basic))
	if err != nil {
		return nil, err
	}

	return swarm.ResolverFromMaDNS{Resolver: resolver}, nil
}

// dnsServerAddr normalises a server to host:port. The platform reports bare
// addresses, and an IPv6 one needs bracketing before it can be dialled.
func dnsServerAddr(server string) (string, error) {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server, nil
	}

	// ParseAddr rather than net.ParseIP, which rejects the zone that a link local
	// address needs to be dialled with.
	if _, err := netip.ParseAddr(server); err != nil {
		return "", fmt.Errorf("not an ip address")
	}

	return net.JoinHostPort(server, dnsPort), nil
}
