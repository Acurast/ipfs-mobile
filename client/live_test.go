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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"
)

// treeDigest walks output, which may be a single file or a UnixFS directory, and
// returns the total bytes together with a stable digest of the whole tree.
//
// The default CID is a directory, so this also gives the directory branch of
// files.WriteTo its only coverage anywhere in the suite.
func treeDigest(t *testing.T, root string) (int64, string) {
	t.Helper()

	hash := sha256.New()
	var total int64

	// WalkDir visits lexically, so the digest is deterministic.
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		fmt.Fprintf(hash, "%s:%d\n", relative, len(content))
		hash.Write(content)
		total += int64(len(content))

		return nil
	})
	if err != nil {
		t.Fatalf("walking the downloaded tree: %v", err)
	}

	return total, hex.EncodeToString(hash.Sum(nil))
}

// Mirrors Constants.IPFS_BOOTSTRAP_NODES in acurast-data-transmitter, minus the
// Pinata bitswap gateway. These are DHT bootstrap nodes: they hold no content
// themselves, so retrieving anything through them exercises the DHT provider
// lookup end to end.
var defaultLivePeers = []string{
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
}

// The readme shipped with every kubo init, so it is about the most widely
// replicated and announced content on the public network.
//
// Deliberately not the processor's own health-check CID
// (QmSHJwK2EW2Ruq4bZUTNyMxw37Rx6bZjyaqwXrJ7i63EhQ): measured against the public
// DHT that one resolves to zero providers, because whoever pins it serves it
// over a direct bitswap connection without publishing provider records. It is
// therefore unreachable by content routing, and useless for testing it.
const defaultLiveCID = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"

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

	total, _ := treeDigest(t, output)
	if total == 0 {
		t.Error("downloaded nothing")
	}

	t.Logf("fetched %d bytes for %s", total, liveCID())
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

	coldSize, coldDigest := treeDigest(t, filepath.Join(dir, "cold"))
	warmSize, warmDigest := treeDigest(t, filepath.Join(dir, "warm"))

	if coldDigest != warmDigest || coldSize != warmSize {
		t.Errorf("the two downloads disagree: %d bytes %s vs %d bytes %s", coldSize, coldDigest, warmSize, warmDigest)
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
