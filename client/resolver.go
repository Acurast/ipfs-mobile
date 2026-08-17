package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	madns "github.com/multiformats/go-multiaddr-dns"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
)

const dnsPort = "53"

// How long a TXT answer is reused. The records behind a /dnsaddr/ address name
// bootstrap peers and change on the order of months, while libp2p looks them up
// again for every dial - the same few names, over and over.
const txtAnswerLifetime = time.Hour

// How many answers are held. A peer chooses the names in the records it serves,
// and those name further records, so the keys here are not all ours to count on.
const txtAnswerLimit = 64

// txtAnswers remembers what /dnsaddr/ lookups resolved to. Process-wide for the
// same reason gatewayCooldowns is: a caller that builds a Client per download
// would otherwise drop the answers exactly when the next download is about to
// ask for the same bootstrap names again.
var txtAnswers = newTXTCache()

// newResolver builds the resolver libp2p resolves addresses with. None returns
// none, leaving libp2p its own.
//
// A /dnsaddr/ address needs a TXT lookup, and Android publishes no resolver
// configuration to build one from - plain host lookups go through the platform
// and survive, TXT ones do not. Only those are sent to the given servers.
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

	resolver, err := madns.NewResolver(madns.WithDefaultResolver(txtOverride{servers: basic}))
	if err != nil {
		return nil, err
	}

	return swarm.ResolverFromMaDNS{Resolver: resolver}, nil
}

// txtOverride sends TXT lookups to the servers it was given and leaves the rest
// to the platform, which answers those and honours the system's own settings -
// private DNS among them.
type txtOverride struct {
	servers *net.Resolver
}

func (resolver txtOverride) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if records, ok := txtAnswers.lookup(name); ok {
		return records, nil
	}

	records, err := resolver.servers.LookupTXT(ctx, name)
	if err != nil {
		return nil, err
	}

	txtAnswers.keep(name, records)

	return records, nil
}

type txtCache struct {
	mutex   sync.Mutex
	answers map[string]txtAnswer
}

type txtAnswer struct {
	records []string
	until   time.Time
}

func newTXTCache() *txtCache {
	return &txtCache{answers: make(map[string]txtAnswer)}
}

func (cache *txtCache) lookup(name string) ([]string, bool) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	answer, held := cache.answers[name]
	if !held || time.Now().After(answer.until) {
		return nil, false
	}

	return answer.records, true
}

// keep stores an answer. A full cache of live answers takes no more, since
// evicting one a peer cannot choose in favour of one it can is the wrong way
// round.
func (cache *txtCache) keep(name string, records []string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if len(cache.answers) >= txtAnswerLimit {
		for cached, answer := range cache.answers {
			if time.Now().After(answer.until) {
				delete(cache.answers, cached)
			}
		}

		if len(cache.answers) >= txtAnswerLimit {
			return
		}
	}

	cache.answers[name] = txtAnswer{records: records, until: time.Now().Add(txtAnswerLifetime)}
}

func (resolver txtOverride) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
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
