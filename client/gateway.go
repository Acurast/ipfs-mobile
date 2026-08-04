package client

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	multihash "github.com/multiformats/go-multihash"
)

// parseGateways turns gateway URLs into peers that the HTTP exchange can talk
// to, so a gateway is fetched from as a peer rather than as a special case.
//
// Every gateway needs a peer ID because that is how bitswap keys a peer, but a
// trustless gateway has no libp2p identity to offer. httpnet never asks for one:
// it probes the URL and remembers what answered. So the ID here is a
// deterministic synthetic derived from the URL, used purely as a map key.
//
// Nothing trusts it. Safety on this path comes from hashing each returned block
// against the CID that was requested, which is what makes fetching from an
// anonymous HTTP endpoint sound at all.
func parseGateways(gateways []string) ([]peer.AddrInfo, error) {
	peers := make([]peer.AddrInfo, 0, len(gateways))

	for _, gateway := range gateways {
		addr, err := gatewayMultiaddr(gateway)
		if err != nil {
			fmt.Printf("skipping invalid gateway %q: %s\n", gateway, err)
			continue
		}

		id, err := gatewayPeerID(gateway)
		if err != nil {
			fmt.Printf("skipping invalid gateway %q: %s\n", gateway, err)
			continue
		}

		peers = append(peers, peer.AddrInfo{ID: id, Addrs: []multiaddr.Multiaddr{addr}})
	}

	if len(gateways) > 0 && len(peers) == 0 {
		return nil, fmt.Errorf("none of the %d configured gateways is a valid URL", len(gateways))
	}

	return peers, nil
}

// gatewayMultiaddr renders a gateway URL in the form the HTTP exchange expects,
// for example https://ipfs.io -> /dns4/ipfs.io/tcp/443/https.
func gatewayMultiaddr(gateway string) (multiaddr.Multiaddr, error) {
	parsed, err := url.Parse(gateway)
	if err != nil {
		return nil, err
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("no host in %q", gateway)
	}

	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return nil, fmt.Errorf("unsupported scheme %q, want http or https", parsed.Scheme)
		}
	}

	// boxo rejects plaintext http for anything that is not loopback or private,
	// so a misconfigured public gateway fails here rather than silently
	// downgrading.
	protocol := "dns4"
	if ip := net.ParseIP(host); ip != nil {
		protocol = "ip4"
		if ip.To4() == nil {
			protocol = "ip6"
		}
	}

	return multiaddr.NewMultiaddr(fmt.Sprintf("/%s/%s/tcp/%s/%s", protocol, host, port, parsed.Scheme))
}

// gatewayPeerID derives a stable identifier for a gateway from its URL. See
// parseGateways for why a synthetic one is sound here.
func gatewayPeerID(gateway string) (peer.ID, error) {
	sum := sha256.Sum256([]byte(gateway))

	encoded, err := multihash.Encode(sum[:], multihash.SHA2_256)
	if err != nil {
		return "", err
	}

	return peer.ID(encoded), nil
}
