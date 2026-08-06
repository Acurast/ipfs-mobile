package client

import (
	"bytes"
	"context"
	"io"
	"ipfs-mobile/internal/testpeer"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
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

	peers := parsePeers([]string{
		"/ip4/127.0.0.1/tcp/1/p2p/" + id,
		"/ip4/127.0.0.2/tcp/2/p2p/" + id,
	})

	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(peers))
	}
	if len(peers[0].Addrs) != 2 {
		t.Errorf("got %d addresses for the peer, want 2", len(peers[0].Addrs))
	}
}

func TestDialAllSettlesWithNothingConnectedWhenNoneAnswer(t *testing.T) {
	peers := parsePeers([]string{deadPeer})

	host, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	group := dialAll(ctx, peers, host.Connect, peerName)

	select {
	case <-group.done:
	case <-ctx.Done():
		t.Fatal("the dial never settled")
	}

	if group.count() != 0 {
		t.Errorf("connected to %d peers, want 0", group.count())
	}
	if !group.finished() {
		t.Error("finished() is false once every dial has settled")
	}
}

func TestDialAllSignalsTheFirstReachablePeer(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	peers := parsePeers([]string{deadPeer, addr})

	host, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	group := dialAll(ctx, peers, host.Connect, peerName)

	select {
	case <-group.first:
	case <-ctx.Done():
		t.Fatal("a reachable peer was present but nothing reported connecting")
	}
}

// A peer that accepts the connection and then says nothing holds its dial open
// until libp2p gives up. The budget here is shorter than that, so a download
// that waits for the dial cannot finish at all.
func TestStalledPeerDoesNotHoldUpAConnectedGateway(t *testing.T) {
	content := testpeer.Content(4096)
	gateway, root, _ := testpeer.ServeTrustlessGateway(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{testpeer.Stalled(t)},
		Gateways:       []string{gateway},
		// Only the verified path may satisfy this.
		AllowUnverifiedGatewayFallback: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), stalledPeerGivesUpAfter-time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("a connected gateway held the content but the fetch failed: %v", err)
	}

	written, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, content) {
		t.Error("the content written does not match what the gateway served")
	}
}

// How long libp2p spends on a peer that accepts the connection and then goes
// quiet. Asserted rather than assumed, since the test above is only meaningful
// while the budget it sets is the shorter of the two.
const stalledPeerGivesUpAfter = 5 * time.Second

func TestStalledPeerGivesUpWhenExpected(t *testing.T) {
	peers := parsePeers([]string{testpeer.Stalled(t)})

	host, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	group := dialAll(ctx, peers, host.Connect, peerName)

	select {
	case <-group.done:
	case <-ctx.Done():
		t.Fatal("the stalled dial never settled")
	}

	if elapsed := time.Since(started); elapsed < stalledPeerGivesUpAfter {
		t.Errorf("a stalled dial settled after %s, sooner than the assumed %s", elapsed, stalledPeerGivesUpAfter)
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

// A lookup meets far more peers than the configuration names, and the peer that
// holds the content must not be the one evicted to make room for them.
func TestConfiguredPeersAreProtectedFromTrimming(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	peers := parsePeers([]string{addr, deadPeer})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	node, err := startNode(ctx, nodeConfig{peers: peers, disableDHT: true})
	if err != nil {
		t.Fatal(err)
	}
	defer node.close()

	for _, peerInfo := range peers {
		if !node.host.ConnManager().IsProtected(peerInfo.ID, configuredPeerTag) {
			t.Errorf("configured peer %s is not protected from trimming", peerInfo.ID)
		}
	}

	// A peer nobody configured has no such claim.
	otherAddr, _ := testpeer.Serve(t, testpeer.Content(32))

	stranger := parsePeers([]string{otherAddr})
	if node.host.ConnManager().IsProtected(stranger[0].ID, configuredPeerTag) {
		t.Error("a peer that was never configured is protected")
	}
}

// go-libp2p's own watermarks are a server's. Asserted because the cost of
// inheriting them is paid on a phone, where nothing here would notice.
func TestConnectionWatermarksAreNotTheLibp2pDefaults(t *testing.T) {
	const libp2pLow, libp2pHigh = 160, 192

	if highWaterConnections >= libp2pHigh || lowWaterConnections >= libp2pLow {
		t.Errorf(
			"watermarks %d/%d are not below go-libp2p's %d/%d",
			lowWaterConnections, highWaterConnections, libp2pLow, libp2pHigh,
		)
	}
	if lowWaterConnections >= highWaterConnections {
		t.Errorf("low water %d is not below high water %d", lowWaterConnections, highWaterConnections)
	}
	if connectionGracePeriod >= time.Minute {
		t.Errorf("grace period %s does not shorten go-libp2p's minute", connectionGracePeriod)
	}
	// Trimming has to run at least as often as connections become eligible for it,
	// or the grace period alone sets the pace.
	if connectionTrimInterval > 2*connectionGracePeriod {
		t.Errorf("trim interval %s is long next to the %s grace period", connectionTrimInterval, connectionGracePeriod)
	}
}

// A download's own staging directory is never swept, however old it looks. A
// directory's modified time does not advance while a file inside it is written,
// so age alone cannot tell a slow download from an abandoned one.
func TestSweepLeavesADownloadInProgressAlone(t *testing.T) {
	dir := t.TempDir()

	live, release, err := claimScratch(dir, scratchPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	abandoned, err := os.MkdirTemp(dir, scratchPrefix)
	if err != nil {
		t.Fatal(err)
	}

	// Both look equally stale from the outside, which is the whole point.
	old := time.Now().Add(-2 * scratchStaleAfter)
	for _, path := range []string{live, abandoned} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	sweepScratchPrefix(dir, scratchPrefix)

	if _, err := os.Stat(live); os.IsNotExist(err) {
		t.Error("the sweep deleted a staging directory still being written to")
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Error("a staging directory from a dead process survived the sweep")
	}
}

// Releasing removes the directory and gives up the claim, so a later sweep is
// free to act on anything left behind.
func TestReleasingAScratchClaimRemovesIt(t *testing.T) {
	dir := t.TempDir()

	scratch, release, err := claimScratch(dir, scratchPrefix)
	if err != nil {
		t.Fatal(err)
	}

	release()

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Error("the staging directory survived its release")
	}
	if _, live := staging.Load(scratch); live {
		t.Error("the claim outlived the directory")
	}
}

// Noise is offered ahead of TLS, so a peer supporting both agrees on the first
// proposal rather than after rejecting one. The saved round trip is what keeps
// peers with short handshake deadlines reachable.
func TestNoiseIsProposedBeforeTLS(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(64))

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatal(err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	conns := client.node.host.Network().Conns()
	if len(conns) == 0 {
		t.Fatal("no connection to inspect")
	}

	for _, conn := range conns {
		if security := conn.ConnState().Security; security != "/noise" {
			t.Errorf("negotiated %s, want /noise to have been proposed first", security)
		}
	}
}

// A target that fails on every node start is only worth reporting once.
func TestARepeatedConnectFailureIsReportedOnce(t *testing.T) {
	peers := parsePeers([]string{deadPeer})

	// Reports are remembered for the life of the process, so another test dialling
	// this peer first would already have spent the one this asserts on.
	reported.Range(func(key, _ any) bool {
		reported.Delete(key)

		return true
	})

	host, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	captured, restore := captureStdout(t)
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for range 3 {
		group := dialAll(ctx, peers, host.Connect, peerName)
		select {
		case <-group.done:
		case <-ctx.Done():
			t.Fatal("the dial never settled")
		}
	}

	restore()

	if got := strings.Count(captured.String(), "failed to connect to peer"); got != 1 {
		t.Errorf("the same failure was reported %d times, want 1:\n%s", got, captured.String())
	}
}

// captureStdout redirects stdout until the returned function is called, which is
// safe to call more than once.
func captureStdout(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	original := os.Stdout
	os.Stdout = write

	captured := &bytes.Buffer{}
	done := make(chan struct{})

	go func() {
		io.Copy(captured, read)
		close(done)
	}()

	var once sync.Once

	return captured, func() {
		once.Do(func() {
			os.Stdout = original
			write.Close()
			<-done
			read.Close()
		})
	}
}

// Nothing dials this node, and a listener costs a standing interface lookup that
// Android refuses. A port asked for explicitly is still honoured.
func TestNoListenerUnlessAPortIsAskedFor(t *testing.T) {
	quiet, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()

	if addrs := quiet.Addrs(); len(addrs) != 0 {
		t.Errorf("a node with no port configured advertises %v", addrs)
	}

	listening, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listening.Close()

	if addrs := listening.Network().ListenAddresses(); len(addrs) != 0 {
		t.Errorf("a node with no port configured is listening on %v", addrs)
	}
}

func TestAConfiguredPortIsStillHonoured(t *testing.T) {
	host, err := makeHost(45991, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	if !slices.ContainsFunc(host.Network().ListenAddresses(), func(addr multiaddr.Multiaddr) bool {
		return strings.Contains(addr.String(), "45991")
	}) {
		t.Errorf("a configured port was not listened on: %v", host.Network().ListenAddresses())
	}
}

// Only the transports the configured peers speak. The libp2p defaults add two
// more, one of which carries a media stack this never uses.
func TestOnlyTheTransportsInUseAreStarted(t *testing.T) {
	host, err := makeHost(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	swarm, ok := host.Network().(*swarm.Swarm)
	if !ok {
		t.Fatalf("network is %T, not a swarm", host.Network())
	}

	for _, addr := range []string{
		"/ip4/127.0.0.1/tcp/4001",
		"/ip4/127.0.0.1/udp/4001/quic-v1",
		"/dns4/example.invalid/tcp/443/wss",
	} {
		if swarm.TransportForDialing(multiaddr.StringCast(addr)) == nil {
			t.Errorf("no transport for %s, which configured peers use", addr)
		}
	}

	for _, addr := range []string{
		"/ip4/127.0.0.1/udp/4001/quic-v1/webtransport",
		"/ip4/127.0.0.1/udp/4001/webrtc-direct",
	} {
		if swarm.TransportForDialing(multiaddr.StringCast(addr)) != nil {
			t.Errorf("a transport was started for %s, which nothing here dials", addr)
		}
	}
}

// A node outlives the download that started it, so one that has lost every
// connection must dial again rather than serve the rest of its idle life with
// nowhere to fetch from.
func TestAPeerlessNodeDialsAgain(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	// No DHT, so nothing else would dial these peers again.
	client := newClient(t, &Config{BootstrapPeers: []string{addr}, DisableDHT: true})

	warm, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Get(warm, root, filepath.Join(t.TempDir(), "first"), -1); err != nil {
		t.Fatal(err)
	}

	client.mutex.Lock()
	node := client.node
	client.mutex.Unlock()

	if node == nil {
		t.Fatal("no node is running")
	}

	// Every connection dropped, as a network change does to an idle node.
	for _, id := range node.host.Network().Peers() {
		if err := node.host.Network().ClosePeer(id); err != nil {
			t.Fatal(err)
		}
	}
	if left := len(node.host.Network().Peers()); left != 0 {
		t.Fatalf("%d connections survived", left)
	}

	// Short enough that a node left peerless cannot pass by waiting.
	brief, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()

	if err := client.Get(brief, root, filepath.Join(t.TempDir(), "second"), -1); err != nil {
		t.Fatalf("a download after losing every peer failed: %v", err)
	}
}
