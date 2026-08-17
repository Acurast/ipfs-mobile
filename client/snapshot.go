package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// How many peers a snapshot keeps. Fewer than a bootstrap meets on its own, so
// seeding from one asks for less of the network than discovering it again would.
const snapshotPeers = 64

// How long a snapshot is worth reading. Addresses go stale, and a device that has
// been away for longer is better off discovering the network than dialling a list
// of peers that have since moved.
const snapshotLifetime = 24 * time.Hour

// readPeerSnapshot returns the peers a previous node left behind, for the DHT to
// start from instead of walking out to find them again. A missing, unreadable or
// stale file is no peers, since the bootstrap works without one.
func readPeerSnapshot(path string) []peer.AddrInfo {
	if path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if time.Since(info.ModTime()) > snapshotLifetime {
		return nil
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	return parsePeers(strings.Fields(string(saved)))
}

// writePeerSnapshot records where the peers this node met can be reached.
//
// Written whole to a neighbouring file and renamed over the old one, so a node
// closing while another reads - or being killed partway, which Android does to
// background work - leaves the previous snapshot rather than half of a new one.
func writePeerSnapshot(path string, peers []peer.AddrInfo) error {
	if path == "" || len(peers) == 0 {
		return nil
	}

	var lines strings.Builder
	for _, peerInfo := range peers {
		for _, addr := range peerInfo.Addrs {
			fmt.Fprintf(&lines, "%s/p2p/%s\n", addr, peerInfo.ID)
		}
	}

	partial, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(partial.Name())

	if _, err := partial.WriteString(lines.String()); err != nil {
		partial.Close()

		return err
	}
	if err := partial.Close(); err != nil {
		return err
	}

	return os.Rename(partial.Name(), path)
}

// snapshotWorth returns the peers of node worth writing down: the ones its DHT
// found usable, at addresses that will still mean something to the next node.
//
// Addresses that need resolving are left out even though they are dialable. The
// snapshot exists to make a start cheap, and a name to look up first is the cost
// it is trying to avoid. Configured peers are left out too, since they arrive in
// the configuration anyway.
func snapshotWorth(node *node, configured []peer.AddrInfo) []peer.AddrInfo {
	if node.dht == nil {
		return nil
	}

	skip := make(map[peer.ID]struct{}, len(configured))
	for _, peerInfo := range configured {
		skip[peerInfo.ID] = struct{}{}
	}

	worth := make([]peer.AddrInfo, 0, snapshotPeers)

	for _, id := range node.dht.RoutingTable().ListPeers() {
		if len(worth) == snapshotPeers {
			break
		}
		if _, configured := skip[id]; configured {
			continue
		}

		keep := snapshotAddrs(node.host.Peerstore().Addrs(id))
		if len(keep) == 0 {
			continue
		}

		worth = append(worth, peer.AddrInfo{ID: id, Addrs: keep})
	}

	return worth
}

// snapshotAddrs keeps the addresses of a peer that are worth writing down.
//
// An address that has to be resolved is dropped even though it is dialable: the
// snapshot exists to make a start cheap, and a name to look up first is the cost
// it is trying to avoid. What the gater would refuse is dropped too, so reading a
// snapshot back cannot reintroduce a dial the node would not make.
func snapshotAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	keep := make([]ma.Multiaddr, 0, len(addrs))

	for _, addr := range addrs {
		if _, err := manet.ToIP(addr); err != nil {
			continue
		}
		if !dialableAddr(addr) {
			continue
		}

		keep = append(keep, addr)
	}

	return keep
}
