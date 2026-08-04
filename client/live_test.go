//go:build integration

// Live tests against the public IPFS network, behind a build tag because they
// need egress and reachable peers:
//
//	go test ./...                     # unit tests only
//	go test -tags=integration ./...   # and these
//
// IPFS_TEST_BOOTSTRAP (";" separated) and IPFS_TEST_CID override the defaults.
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

// treeDigest walks output, a file or a UnixFS directory, and returns its total
// size with a stable digest. The default CID is a directory, so this is also the
// only coverage of that branch of files.WriteTo.
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

// Mirrors Constants.IPFS_BOOTSTRAP_NODES in acurast-data-transmitter, minus
// Pinata. These hold no content themselves, so retrieving through them exercises
// content routing end to end.
var defaultLivePeers = []string{
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
}

// The readme shipped with every kubo init, so about the most widely announced
// content there is. Not the processor's own health-check CID, which resolves to
// zero DHT providers and so cannot exercise the DHT at all.
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

// connectedPeers tells the two usual live failures apart: zero peers means
// connectivity or DNS, peers but no content means the CID is not retrievable.
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

// The second fetch reuses the connected node, so it should be markedly faster.
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

// A call with a deadline has to come back at that deadline against real peers.
func TestLiveTimeoutIsHonoured(t *testing.T) {
	client := liveClient(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), liveShortTimeout)
	defer cancel()

	// Never published: sha256 of a sentinel string as a raw CIDv1, so nobody can
	// serve it and the call runs to the deadline.
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

// A failure here is a problem with the peer list, not with this package, so each
// peer is logged individually.
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

// Served by Pinata and indexed by cid.contact, but absent from the DHT: the case
// delegated routing exists to cover.
const pinataOnlyCID = "QmSHJwK2EW2Ruq4bZUTNyMxw37Rx6bZjyaqwXrJ7i63EhQ"

// Proves the indexer path works against the real network, with Pinata nowhere in
// the bootstrap list.
func TestLiveDelegatedRoutingFindsIndexedOnlyContent(t *testing.T) {
	client, err := New(&Config{
		BootstrapPeers:           livePeers(),
		DelegatedRoutingEndpoint: "https://cid.contact",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	start := time.Now()
	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, pinataOnlyCID, output, -1); err != nil {
		t.Fatalf("indexer lookup failed: %v (%d peers connected)", err, connectedPeers(client))
	}

	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("fetched %d bytes in %v via cid.contact: %q", len(content), time.Since(start).Truncate(time.Millisecond), strings.TrimSpace(string(content)))
}

// The control for the test above.
func TestLiveDHTAloneCannotFindIndexedOnlyContent(t *testing.T) {
	client := liveClient(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := client.Get(ctx, pinataOnlyCID, filepath.Join(t.TempDir(), "out"), -1)
	if err == nil {
		t.Skip("the content is now announced to the DHT; nothing to assert")
	}

	t.Logf("dht alone: %v (%d peers connected)", err, connectedPeers(client))
}

// Mirrors Constants.IPFS_GATEWAYS in acurast-data-transmitter.
var defaultLiveGateways = []string{
	"https://ipfs.io",
	"https://dweb.link",
	"https://gateway.pinata.cloud",
	"https://ipfs.filebase.io",
}

// Surveys which configured gateways actually serve verified blocks, rather than
// asserting it: the answer is a property of someone else's infrastructure, and
// our side is covered offline by TestGatewayServesVerifiedBlocks.
//
// Support is patchy. Subdomain gateways answer the block request with a 301 that
// httpnet does not follow, and at least one sends HTTP/2 headers larger than Go
// accepts. Hence the unverified fallback.
func TestLiveGatewayVerifiedBlockSupport(t *testing.T) {
	var supported []string

	for _, gateway := range defaultLiveGateways {
		client, err := New(&Config{
			BootstrapPeers: livePeers(),
			DisableDHT:     true,
			Gateways:       []string{gateway},
		})
		if err != nil {
			t.Errorf("%s: %v", gateway, err)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = client.Get(ctx, pinataOnlyCID, filepath.Join(t.TempDir(), "out"), -1)
		cancel()
		client.Close()

		if err != nil {
			t.Logf("  %-32s no  (%v)", gateway, err)
			continue
		}

		t.Logf("  %-32s yes", gateway)
		supported = append(supported, gateway)
	}

	t.Logf("%d/%d configured gateways served verified blocks", len(supported), len(defaultLiveGateways))

	if len(supported) == 0 {
		t.Skip("no configured gateway currently serves verified blocks; the unverified fallback is carrying this entirely")
	}
}

// The configuration a processor would actually run.
func TestLiveAllRoutesTogether(t *testing.T) {
	client, err := New(&Config{
		BootstrapPeers:                 livePeers(),
		DelegatedRoutingEndpoint:       "https://cid.contact",
		Gateways:                       defaultLiveGateways,
		AllowUnverifiedGatewayFallback: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	for _, target := range []struct {
		name string
		cid  string
	}{
		{"indexed only (pinata)", pinataOnlyCID},
		{"dht announced", liveCID()},
	} {
		t.Run(target.name, func(t *testing.T) {
			start := time.Now()
			output := filepath.Join(t.TempDir(), "out")
			if err := client.Get(ctx, target.cid, output, -1); err != nil {
				t.Fatalf("%s: %v (%d peers connected)", target.name, err, connectedPeers(client))
			}

			total, _ := treeDigest(t, output)
			t.Logf("%s: %d bytes in %v", target.name, total, time.Since(start).Truncate(time.Millisecond))
		})
	}
}
