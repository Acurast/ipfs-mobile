package client

import (
	"context"
	"ipfs-mobile/internal/testpeer"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Staging directories from a killed process must not accumulate.
func TestDownloadSweepsStaleScratchDirectories(t *testing.T) {
	client, root, _ := servedClient(t, 1024)

	dir := t.TempDir()

	stale := filepath.Join(dir, scratchPrefix+"leftover")
	if err := os.MkdirAll(filepath.Join(stale, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Backdated past the staleness window, since a directory that could belong to
	// a running download is deliberately left alone.
	old := time.Now().Add(-2 * scratchStaleAfter)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(dir, "out"), -1); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale staging directory survived a later download")
	}
}

// Stopping the exchange has to stop both halves, or each node leaks its
// connection-event worker for the life of the process.
func TestNodeCloseStopsTheWholeExchange(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(1024))

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatal(err)
	}

	client.mutex.Lock()
	running := client.node
	client.mutex.Unlock()

	if running == nil {
		t.Fatal("node is not running")
	}
	if running.exchange == nil {
		t.Fatal("the combined exchange was not retained, so close cannot stop it")
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParsePeersMergesAddressesForOneID(t *testing.T) {
	const id = "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

	peers, err := parsePeers([]string{
		"/ip4/127.0.0.1/tcp/1/p2p/" + id,
		"/ip4/127.0.0.2/tcp/2/p2p/" + id,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(peers))
	}
	if len(peers[0].Addrs) != 2 {
		t.Errorf("got %d addresses for the peer, want 2", len(peers[0].Addrs))
	}
}

func TestConnectToPeersFailsWhenNoneAnswer(t *testing.T) {
	peers, err := parsePeers([]string{deadPeer})
	if err != nil {
		t.Fatal(err)
	}

	host, err := makeHost(0)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = connectToPeers(ctx, ctx, host, peers)
	if err == nil {
		t.Fatal("expected an error when no peer answers, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect to any") {
		t.Errorf("err = %v, want it to report that no peer answered", err)
	}
}

func TestConnectToPeersReturnsOnFirstReachable(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	peers, err := parsePeers([]string{deadPeer, addr})
	if err != nil {
		t.Fatal(err)
	}

	host, err := makeHost(0)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := connectToPeers(ctx, ctx, host, peers); err != nil {
		t.Errorf("a reachable peer was present but connect failed: %v", err)
	}
}

func TestDownloadLeavesNoScratchDirectories(t *testing.T) {
	client, root, _ := servedClient(t, 2048)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	if err := client.Get(ctx, root, filepath.Join(dir, "out"), -1); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ipfs-download-") {
			t.Errorf("scratch directory %q was left behind", entry.Name())
		}
	}
}

// With no gateways configured there is no HTTP exchange at all, so an address
// discovered through content routing cannot become an HTTP GET. That is the
// deployed shape today, and it is where the egress exposure would otherwise be.
func TestWithoutGatewaysThereIsNoHTTPExchange(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(1024))

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("libp2p-only retrieval failed: %v", err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node.http != nil {
		t.Error("an HTTP exchange was started with no gateways configured")
	}
}

// Configured gateways do get an HTTP exchange, held to their hosts.
//
// The allowlist itself is only assertable through configuration here: httpnet
// filters on hostname, and every httptest server shares 127.0.0.1, so a rogue
// host cannot be told apart from a legitimate one on loopback.
func TestConfiguredGatewaysFormTheHTTPAllowlist(t *testing.T) {
	gateway, root, _ := testpeer.ServeTrustlessGateway(t, testpeer.Content(1024))

	client := newClient(t, &Config{
		Gateways:   []string{gateway, "https://ipfs.io", "https://dweb.link"},
		DisableDHT: true,
	})

	want := []string{"127.0.0.1", "ipfs.io", "dweb.link"}
	if got := client.config.gatewayHosts; !slices.Equal(got, want) {
		t.Errorf("gatewayHosts = %v, want %v", got, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("retrieval through an allowlisted gateway failed: %v", err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node.http == nil {
		t.Error("no HTTP exchange despite configured gateways")
	}
}
