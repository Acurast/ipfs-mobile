package client

import (
	"context"
	"ipfs-mobile/internal/testpeer"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	ma "github.com/multiformats/go-multiaddr"
)

func snapshotFile(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "peers")
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := snapshotFile(t)

	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatal(err)
	}

	written := []peer.AddrInfo{{
		ID: id,
		Addrs: []ma.Multiaddr{
			ma.StringCast("/ip4/104.131.131.82/tcp/4001"),
			ma.StringCast("/ip4/104.131.131.82/udp/4001/quic-v1"),
		},
	}}

	if err := writePeerSnapshot(path, written); err != nil {
		t.Fatal(err)
	}

	read := readPeerSnapshot(path)
	if len(read) != 1 {
		t.Fatalf("read %d peers, want 1", len(read))
	}
	if read[0].ID != id {
		t.Errorf("read peer %s, want %s", read[0].ID, id)
	}
	// Both addresses of one peer come back under that single entry, which is what
	// lets a seed be tried over either transport.
	if len(read[0].Addrs) != 2 {
		t.Errorf("read %d addresses, want 2", len(read[0].Addrs))
	}
}

// A snapshot no node has refreshed is worse than none: its peers have had time to
// move, so a start would spend its dials on a list that has gone cold.
func TestASnapshotIsIgnoredOnceStale(t *testing.T) {
	path := snapshotFile(t)

	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatal(err)
	}

	err = writePeerSnapshot(path, []peer.AddrInfo{
		{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/104.131.131.82/tcp/4001")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(readPeerSnapshot(path)) == 0 {
		t.Fatal("a fresh snapshot was ignored")
	}

	stale := time.Now().Add(-2 * snapshotLifetime)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	if peers := readPeerSnapshot(path); len(peers) != 0 {
		t.Errorf("a stale snapshot yielded %d peers, want none", len(peers))
	}
}

// Nothing to read is a first start, not a failure.
func TestAMissingSnapshotIsNoPeers(t *testing.T) {
	if peers := readPeerSnapshot(filepath.Join(t.TempDir(), "absent")); peers != nil {
		t.Errorf("a missing snapshot yielded %d peers", len(peers))
	}
	if peers := readPeerSnapshot(""); peers != nil {
		t.Error("an unset path yielded peers")
	}
}

// Junk in the file costs the start its head start, never the start itself.
func TestACorruptSnapshotIsSurvivable(t *testing.T) {
	path := snapshotFile(t)

	if err := os.WriteFile(path, []byte("not-a-multiaddr\n\x00\x01\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if peers := readPeerSnapshot(path); len(peers) != 0 {
		t.Errorf("a corrupt snapshot yielded %d peers", len(peers))
	}
}

// Writing whole and renaming means a reader sees one snapshot or the other, so a
// node killed partway leaves the previous one rather than half of a new one.
func TestWritingASnapshotLeavesNoPartialFile(t *testing.T) {
	path := snapshotFile(t)

	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatal(err)
	}

	for round := range 3 {
		err := writePeerSnapshot(path, []peer.AddrInfo{
			{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/104.131.131.82/tcp/4001")}},
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	left, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		names := make([]string, 0, len(left))
		for _, entry := range left {
			names = append(names, entry.Name())
		}

		t.Errorf("the directory holds %s, want just the snapshot", strings.Join(names, ", "))
	}
}

// The point of the whole thing: a node that met peers through the DHT writes them
// down on the way out, so the next one has somewhere to start.
func TestClosingANodeRecordsThePeersItMet(t *testing.T) {
	content := testpeer.Content(4096)
	bootstrapAddr, root := testpeer.ServeViaDHT(t, content)

	path := snapshotFile(t)

	client := newClient(t, &Config{
		BootstrapPeers:   []string{bootstrapAddr},
		PeerSnapshotPath: path,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Written by close, since that is when the routing table is still readable and
	// the node is known to be finished with it.
	client.Close()

	recorded := readPeerSnapshot(path)
	if len(recorded) == 0 {
		t.Fatal("closing a node that used the dht recorded no peers")
	}

	configured := parsePeers([]string{bootstrapAddr})
	for _, peerInfo := range recorded {
		if peerInfo.ID == configured[0].ID {
			t.Error("a configured peer was recorded, though it arrives in the configuration anyway")
		}
		if len(peerInfo.Addrs) == 0 {
			t.Errorf("%s was recorded with no addresses", peerInfo.ID)
		}
	}
}

// A snapshot is a head start, not a dependency: a node handed peers that have all
// gone still fetches through the ones it was configured with.
func TestASnapshotOfDeadPeersDoesNotBreakAStart(t *testing.T) {
	path := snapshotFile(t)

	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatal(err)
	}

	// Reserved for documentation, so nothing answers.
	err = writePeerSnapshot(path, []peer.AddrInfo{
		{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/192.0.2.1/tcp/4001")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	addr, root := testpeer.Serve(t, testpeer.Content(2048))

	client := newClient(t, &Config{
		BootstrapPeers:   []string{addr},
		PeerSnapshotPath: path,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("a stale snapshot got in the way of a download: %v", err)
	}
}

// The snapshot exists to make a start cheap, so an address that has to be resolved
// first - or one the gater would refuse anyway - is not worth keeping.
func TestSnapshotKeepsOnlyAddressesWorthDialling(t *testing.T) {
	for _, test := range []struct {
		addr string
		keep bool
	}{
		{"/ip4/104.131.131.82/tcp/4001", true},
		{"/ip4/104.131.131.82/udp/4001/quic-v1", true},
		{"/ip6/2606:4700:4700::1111/tcp/4001", true},

		// A name would cost the next start the lookup this is here to avoid.
		{"/dns4/example.com/tcp/443/wss", false},
		{"/dnsaddr/bootstrap.libp2p.io/tcp/4001", false},

		// Refused by the gater, so keeping it would only waste a dial.
		{"/ip4/10.0.0.1/tcp/4001", false},
		{"/ip4/192.168.1.5/tcp/4001", false},
	} {
		kept := snapshotAddrs([]ma.Multiaddr{ma.StringCast(test.addr)})

		if got := len(kept) == 1; got != test.keep {
			t.Errorf("%s kept = %v, want %v", test.addr, got, test.keep)
		}
	}
}
