package client

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ipfs-mobile/internal/testpeer"
)

const (
	unreachableCID = testpeer.UnreachableCID
	deadPeer       = testpeer.DeadPeer
)

func newClient(t *testing.T, config *Config) *Client {
	t.Helper()

	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

// servedClient returns a Client bootstrapped against an in-process peer serving
// content, along with the root CID and the content itself.
func servedClient(t *testing.T, size int) (*Client, string, []byte) {
	t.Helper()

	content := testpeer.Content(size)
	addr, root := testpeer.Serve(t, content)

	return newClient(t, &Config{BootstrapPeers: []string{addr}}), root, content
}

// blockedClient returns a Client connected to a real peer that does not hold
// unreachableCID. Connecting succeeds, so the download itself is what blocks -
// which is what the timeout and teardown tests need to exercise.
func blockedClient(t *testing.T, config *Config) *Client {
	t.Helper()

	addr, _ := testpeer.Serve(t, testpeer.Content(64))
	config.BootstrapPeers = []string{addr}

	return newClient(t, config)
}

func TestGetDownloadsContent(t *testing.T) {
	client, root, content := servedClient(t, 4096)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("Get: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}
}

// The node is started once and reused, so bootstrap peers are not re-dialled on
// every fetch. That reuse is the whole point of the handle.
func TestGetReusesTheNodeAcrossDownloads(t *testing.T) {
	client, root, content := servedClient(t, 2048)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	for i := range 3 {
		output := filepath.Join(dir, "out")
		if err := client.Get(ctx, root, output, -1); err != nil {
			t.Fatalf("download %d: %v", i, err)
		}

		got, err := os.ReadFile(output)
		if err != nil {
			t.Fatalf("download %d: reading output: %v", i, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("download %d: content mismatch", i)
		}
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node == nil {
		t.Error("node was torn down between downloads")
	}
	if client.inflight != 0 {
		t.Errorf("inflight = %d after all downloads finished, want 0", client.inflight)
	}
}

func TestGetConcurrentDownloadsShareOneNode(t *testing.T) {
	client, root, content := servedClient(t, 2048)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()
	errs := make([]error, 8)

	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = client.Get(ctx, root, filepath.Join(dir, string(rune('a'+i))), -1)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("download %d: %v", i, err)
			continue
		}

		got, err := os.ReadFile(filepath.Join(dir, string(rune('a'+i))))
		if err != nil {
			t.Errorf("download %d: %v", i, err)
			continue
		}
		if !bytes.Equal(got, content) {
			t.Errorf("download %d: content mismatch", i)
		}
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.inflight != 0 {
		t.Errorf("inflight = %d, want 0", client.inflight)
	}
}

func TestGetEnforcesSizeLimit(t *testing.T) {
	client, root, content := servedClient(t, 8192)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	err := client.Get(ctx, root, output, int64(len(content)/2))

	if err == nil {
		t.Fatal("expected the size limit to be enforced, got nil")
	}
	if !strings.HasPrefix(err.Error(), "size limit exceeded") {
		t.Errorf("expected a size limit error, got %v", err)
	}

	// The Kotlin wrapper keys SizeLimitExceededException off this prefix, and it
	// must not have written anything.
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Error("output was written despite exceeding the size limit")
	}
}

func TestGetAllowsContentAtTheSizeLimit(t *testing.T) {
	client, root, content := servedClient(t, 4096)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, int64(len(content))); err != nil {
		t.Fatalf("content exactly at the limit was rejected: %v", err)
	}
}

// A timed out download must not leave a partial file behind, and must not
// clobber content already at the output path.
func TestGetLeavesOutputIntactOnTimeout(t *testing.T) {
	client := blockedClient(t, &Config{})

	output := filepath.Join(t.TempDir(), "out")
	existing := []byte("previous content")
	if err := os.WriteFile(output, existing, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := client.Get(ctx, unreachableCID, output, -1); err == nil {
		t.Fatal("expected an error, got nil")
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("output was destroyed by the failed download: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("output = %q, want the original %q", got, existing)
	}
}

// The deadline has to bound the whole call, including node startup, which dials
// bootstrap peers and is usually the slowest part of a cold fetch. The peer here
// stalls mid-handshake, so the call is stuck in startup when the deadline hits.
func TestGetIsBoundedByDeadlineIncludingStartup(t *testing.T) {
	client := newClient(t, &Config{BootstrapPeers: []string{testpeer.Stalled(t)}})

	timeout := 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	err := client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if elapsed > 2*timeout {
		t.Errorf("Get took %v with a %v deadline (%.1fx over)", elapsed, timeout, float64(elapsed)/float64(timeout))
	}
}

// When no peer is reachable the call reports that immediately rather than
// blocking until the deadline. The Kotlin wrapper falls back to HTTP gateways on
// failure, so a fast, specific error gets that fallback going sooner.
func TestGetFailsFastWhenNoPeerIsReachable(t *testing.T) {
	client := newClient(t, &Config{BootstrapPeers: []string{deadPeer}})

	timeout := 20 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	err := client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect to any") {
		t.Errorf("err = %v, want it to name the connection failure", err)
	}
	if elapsed > timeout/2 {
		t.Errorf("took %v to report an unreachable peer, expected it to fail fast", elapsed)
	}
}

func TestGetReportsDeadlineAsTimeout(t *testing.T) {
	client := newClient(t, &Config{BootstrapPeers: []string{testpeer.Stalled(t)}})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	// The Kotlin wrapper surfaces this message, so the wording is load bearing.
	if err.Error() != "timeout" {
		t.Errorf("err = %q, want %q", err.Error(), "timeout")
	}
}

// Cancellation is not a timeout and must not be reported as one.
func TestGetReportsCancellationDistinctly(t *testing.T) {
	client := newClient(t, &Config{BootstrapPeers: []string{testpeer.Stalled(t)}})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	err := client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A malformed CID used to reach cid.MustParse on a background goroutine, where
// the panic was unrecoverable and took the process with it.
func TestGetRejectsMalformedCID(t *testing.T) {
	client, _, _ := servedClient(t, 512)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := client.Get(ctx, "not-a-cid", filepath.Join(t.TempDir(), "out"), -1)
	if err == nil {
		t.Fatal("expected an error for a malformed cid, got nil")
	}
	if !strings.Contains(err.Error(), "invalid cid") {
		t.Errorf("expected an invalid cid error, got %v", err)
	}
}

func TestCloseIsIdempotentAndBlocksFurtherUse(t *testing.T) {
	client, root, _ := servedClient(t, 1024)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("Get before Close: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out2"), -1)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v, want ErrClosed", err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node != nil {
		t.Error("node still running after Close")
	}
}

// Close must not hang while a download is in flight, and must not deadlock
// against it. The download itself is expected to fail once the node goes away.
func TestCloseDuringDownloadDoesNotDeadlock(t *testing.T) {
	client := blockedClient(t, &Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := make(chan struct{})
	finished := make(chan error, 1)

	go func() {
		close(started)
		finished <- client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	}()

	<-started
	time.Sleep(100 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close deadlocked against an in-flight download")
	}

	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("in-flight download never returned after Close")
	}
}

func TestIdleTimeoutClosesTheNodeAndItRestarts(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{addr},
		IdleTimeout:    150 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	if err := client.Get(ctx, root, filepath.Join(dir, "first"), -1); err != nil {
		t.Fatalf("first download: %v", err)
	}

	// Wait past the idle window; the node should shut itself down.
	deadline := time.Now().Add(10 * time.Second)
	for {
		client.mutex.Lock()
		idleClosed := client.node == nil
		client.mutex.Unlock()

		if idleClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node was still running well past the idle timeout")
		}

		time.Sleep(20 * time.Millisecond)
	}

	// The Client stays usable: the next download brings a new node up.
	if err := client.Get(ctx, root, filepath.Join(dir, "second"), -1); err != nil {
		t.Fatalf("download after idle shutdown: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch after the node restarted")
	}
}

// With no idle timeout the caller owns the lifetime, so the node must stay up.
func TestIdleTimeoutDisabledKeepsTheNodeRunning(t *testing.T) {
	client, root, _ := servedClient(t, 1024)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("Get: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node == nil {
		t.Error("node shut down even though IdleTimeout is disabled")
	}
	if client.timer != nil {
		t.Error("idle timer was armed even though IdleTimeout is disabled")
	}
}

// An in-flight download must keep the idle timer from tearing the node out from
// under it.
func TestIdleTimeoutDoesNotFireDuringADownload(t *testing.T) {
	client := blockedClient(t, &Config{IdleTimeout: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	go func() {
		client.Get(ctx, unreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	}()

	// Well past the idle window, but the download is still running.
	time.Sleep(300 * time.Millisecond)

	client.mutex.Lock()
	inflight := client.inflight
	client.mutex.Unlock()

	if inflight == 0 {
		t.Skip("download finished before the check; nothing to assert")
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node == nil {
		t.Error("idle timer closed the node while a download was in flight")
	}
}

func TestNewRejectsUnusableBootstrapLists(t *testing.T) {
	tests := []struct {
		name  string
		peers []string
		want  string
	}{
		{"empty", []string{}, "no bootstrap peers configured"},
		{"all invalid", []string{"", "not-a-multiaddr"}, "none of the 2 configured bootstrap peers"},
		{"missing peer id", []string{"/ip4/127.0.0.1/tcp/4001"}, "none of the 1 configured bootstrap peers"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(&Config{BootstrapPeers: test.peers})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("err = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

// One malformed entry must not discard the rest of the list.
func TestNewSkipsInvalidPeersButKeepsValidOnes(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{"", "not-a-multiaddr", addr},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("the valid peer was discarded along with the invalid ones: %v", err)
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

	err = connectToPeers(ctx, host, peers)
	if err == nil {
		t.Fatal("expected an error when no peer answers, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect to any") {
		t.Errorf("err = %v, want it to report that no peer answered", err)
	}
}

// connectToPeers returns as soon as one peer answers rather than waiting for
// every dial, so one dead entry must not hold up a working one.
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

	if err := connectToPeers(ctx, host, peers); err != nil {
		t.Errorf("a reachable peer was present but connect failed: %v", err)
	}
}

func TestPackageGetFetchesAndCleansUp(t *testing.T) {
	content := testpeer.Content(2048)
	addr, root := testpeer.Serve(t, content)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	err := Get(ctx, root, output, &Config{BootstrapPeers: []string{addr}}, -1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
}

// The staging directory used for the atomic write must never be left behind.
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
