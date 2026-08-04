//go:build integration

// Live tests against the public IPFS network. They need working egress and
// reachable bootstrap peers, so they are kept behind a build tag:
//
//	go test ./...                     # unit tests only
//	go test -tags=integration ./...   # and these
//
// Override the defaults with IPFS_TEST_BOOTSTRAP (a ";" separated peer list)
// and IPFS_TEST_CID.
package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Mirrors Constants.IPFS_BOOTSTRAP_NODES in acurast-data-transmitter, so these
// tests exercise what the processor actually dials in production.
var defaultLivePeers = []string{
	"/dns4/bitswap.pinata.cloud/tcp/3000/ws/p2p/Qma8ddFEQWEU8ijWvdxXm3nxU7oHsRtCykAaVz8WUYhiKn",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
}

// The processor's own health-check content, fetched by HeartbeatService on every
// heartbeat, so it is as reliably pinned as anything this project depends on.
const defaultLiveCID = "QmSHJwK2EW2Ruq4bZUTNyMxw37Rx6bZjyaqwXrJ7i63EhQ"

// Public IPFS retrieval is slow and variable; these are deliberately generous.
const (
	liveTimeout      = 90 * time.Second
	liveShortTimeout = 3 * time.Second
)

func livePeers() []string {
	if env := os.Getenv("IPFS_TEST_BOOTSTRAP"); env != "" {
		return strings.Split(env, ";")
	}

	return defaultLivePeers
}

func liveCID() string {
	if env := os.Getenv("IPFS_TEST_CID"); env != "" {
		return env
	}

	return defaultLiveCID
}

// connectedPeers reports how many peers the client's node currently holds.
//
// Live failures are almost always one of two things, and this tells them apart:
// zero peers means connectivity or DNS, peers but no content means the CID is
// not retrievable right now. Without it every failure just reads "timeout".
func connectedPeers(client *Client) int {
	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node == nil {
		return 0
	}

	return len(client.node.host.Network().Peers())
}

func liveClient(t *testing.T, idle time.Duration) *Client {
	t.Helper()

	client, err := New(&Config{BootstrapPeers: livePeers(), IdleTimeout: idle})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

func TestLiveDownload(t *testing.T) {
	client := liveClient(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, liveCID(), output, -1); err != nil {
		t.Fatalf("live download failed: %v (%d peers connected)", err, connectedPeers(client))
	}

	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Error("downloaded an empty file")
	}

	t.Logf("fetched %d bytes for %s", len(content), liveCID())
}

// The second fetch reuses the already connected node, which is the point of the
// handle: it should be markedly faster than the cold one.
func TestLiveDownloadReusesConnections(t *testing.T) {
	client := liveClient(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*liveTimeout)
	defer cancel()

	dir := t.TempDir()

	start := time.Now()
	if err := client.Get(ctx, liveCID(), filepath.Join(dir, "cold"), -1); err != nil {
		t.Fatalf("cold download: %v (%d peers connected)", err, connectedPeers(client))
	}
	cold := time.Since(start)

	start = time.Now()
	if err := client.Get(ctx, liveCID(), filepath.Join(dir, "warm"), -1); err != nil {
		t.Fatalf("warm download: %v", err)
	}
	warm := time.Since(start)

	t.Logf("cold = %v, warm = %v", cold, warm)

	first, err := os.ReadFile(filepath.Join(dir, "cold"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "warm"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the two downloads disagree on the content")
	}
}

// The bug this all started from: against real peers, a call with a deadline has
// to come back at that deadline rather than running long.
func TestLiveTimeoutIsHonoured(t *testing.T) {
	client := liveClient(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), liveShortTimeout)
	defer cancel()

	// The CID of content that has never been published: sha256 of a sentinel
	// string, as raw CIDv1. Nobody can serve it, so the call runs until the
	// deadline rather than finishing early.
	const unfindable = "bafkreif3r4evobsxhtfza6zgi7jz2jqv4bqxqs5wn6hies6ejzchufc66y"

	start := time.Now()
	err := client.Get(ctx, unfindable, filepath.Join(t.TempDir(), "out"), -1)
	elapsed := time.Since(start)

	if err == nil {
		t.Skip("content unexpectedly retrievable; nothing to assert")
	}

	t.Logf("returned %v after %v", err, elapsed)

	if elapsed > 3*liveShortTimeout {
		t.Errorf("took %v with a %v deadline (%.1fx over)", elapsed, liveShortTimeout, float64(elapsed)/float64(liveShortTimeout))
	}
}

func TestLiveIdleShutdownAndRestart(t *testing.T) {
	client := liveClient(t, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*liveTimeout)
	defer cancel()

	dir := t.TempDir()
	if err := client.Get(ctx, liveCID(), filepath.Join(dir, "first"), -1); err != nil {
		t.Fatalf("first download: %v (%d peers connected)", err, connectedPeers(client))
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		client.mutex.Lock()
		down := client.node == nil
		client.mutex.Unlock()

		if down {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node was still running well past the idle timeout")
		}

		time.Sleep(100 * time.Millisecond)
	}

	start := time.Now()
	if err := client.Get(ctx, liveCID(), filepath.Join(dir, "second"), -1); err != nil {
		t.Fatalf("download after idle shutdown: %v (%d peers connected, after %v)", err, connectedPeers(client), time.Since(start))
	}
}

// Every configured bootstrap peer should be dialable. Failures here are a
// problem with the peer list rather than with this package, so they are logged
// individually.
func TestLiveBootstrapPeersAreReachable(t *testing.T) {
	peers, err := parsePeers(livePeers())
	if err != nil {
		t.Fatalf("parsing the configured peer list: %v", err)
	}

	host, err := makeHost(0)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	var reachable int
	for _, peerInfo := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := host.Connect(ctx, peerInfo)
		cancel()

		if err != nil {
			t.Logf("unreachable: %s (%v)", peerInfo.ID, err)
			continue
		}

		reachable++
		t.Logf("reachable:   %s", peerInfo.ID)
	}

	t.Logf("%d/%d bootstrap peers reachable", reachable, len(peers))

	if reachable == 0 {
		t.Error("no configured bootstrap peer is reachable")
	}
}
