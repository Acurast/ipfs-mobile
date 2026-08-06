package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// servedClient returns a Client bootstrapped against a peer serving content.
func servedClient(t *testing.T, size int) (*Client, string, []byte) {
	t.Helper()

	content := testpeer.Content(size)
	addr, root := testpeer.Serve(t, content)

	return newClient(t, &Config{BootstrapPeers: []string{addr}}), root, content
}

// blockedClient connects to a real peer that does not hold unreachableCID, so
// the download blocks rather than the connection.
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

// A failed download must not clobber content already at the output path.
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

// The peer stalls mid-handshake, so the call is still in startup when the
// deadline hits.
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
	if !strings.Contains(err.Error(), "could be reached") {
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

// The in-flight download is expected to fail once the node goes away.
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
		{"empty", []string{}, "nowhere to fetch from"},
		{"all invalid", []string{"", "not-a-multiaddr"}, "none of the 2 configured"},
		{"missing peer id", []string{"/ip4/127.0.0.1/tcp/4001"}, "none of the 1 configured"},
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

// With no caller deadline the verified phase has to be bounded anyway, or it runs
// forever and the fallback below it is unreachable.
func TestFallbackReachableWithoutCallerDeadline(t *testing.T) {
	content := testpeer.Content(2048)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)
	gateway, requests := testpeer.ServeWholeFileGateway(t, root, content)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 500 * time.Millisecond,
	})

	// Deliberately no deadline.
	done := make(chan error, 1)
	go func() {
		done <- client.Get(context.Background(), root, filepath.Join(t.TempDir(), "out"), -1)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fallback did not run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Get never returned, so the verified phase was unbounded")
	}

	if requests.Load() == 0 {
		t.Error("the gateway was never asked")
	}
}

// A caller deadline may shorten the primary phase but never extend it, so some
// of that deadline always survives for the fallback.
func TestPrimaryTimeoutIsAnUpperBound(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))
	gateway, _, _ := testpeer.ServeTrustlessGateway(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{addr},
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 2 * time.Second,
	})

	tests := []struct {
		name          string
		callerTimeout time.Duration
		want          time.Duration
	}{
		// The share of a generous deadline exceeds the configured bound, so the
		// bound wins and the rest is left for the fallback.
		{"generous deadline", time.Minute, 2 * time.Second},

		// A tight deadline's share is smaller, so it wins instead.
		{"tight deadline", time.Second, 750 * time.Millisecond},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), test.callerTimeout)
			defer cancel()

			primary, release := client.primaryDeadline(ctx)
			defer release()

			deadline, ok := primary.Deadline()
			if !ok {
				t.Fatal("the primary phase was left unbounded")
			}

			budget := time.Until(deadline)
			if budget > test.want+250*time.Millisecond {
				t.Errorf("primary budget = %v, want about %v", budget, test.want)
			}
		})
	}
}

// With no caller deadline the configured bound is what applies.
func TestPrimaryTimeoutAppliesWithoutCallerDeadline(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))
	gateway, _, _ := testpeer.ServeTrustlessGateway(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{addr},
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 3 * time.Second,
	})

	primary, release := client.primaryDeadline(context.Background())
	defer release()

	deadline, ok := primary.Deadline()
	if !ok {
		t.Fatal("the primary phase was left unbounded")
	}

	if budget := time.Until(deadline); budget > 3*time.Second+250*time.Millisecond {
		t.Errorf("primary budget = %v, want about 3s", budget)
	}
}

func TestTimeoutsDefaultWhenUnset(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	if client.primaryTimeout != defaultPrimaryTimeout {
		t.Errorf("primaryTimeout = %v, want %v", client.primaryTimeout, defaultPrimaryTimeout)
	}
	if client.fallbackStepTimeout != defaultFallbackStepTimeout {
		t.Errorf("fallbackStepTimeout = %v, want %v", client.fallbackStepTimeout, defaultFallbackStepTimeout)
	}
}

// A closed Client must not reach the network, and must not report success.
func TestGetAfterCloseDoesNotFallBack(t *testing.T) {
	content := testpeer.Content(1024)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)

	var asked atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	t.Cleanup(gateway.Close)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway.URL},
		AllowUnverifiedGatewayFallback: true,
	})

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if asked.Load() != 0 {
		t.Errorf("a closed client made %d gateway requests", asked.Load())
	}
}

// A gateway-only Client is a valid configuration, and so is an indexer-only one.
func TestNewAcceptsGatewayOnlyAndIndexerOnly(t *testing.T) {
	content := testpeer.Content(1024)
	gateway, root, blockRequests := testpeer.ServeTrustlessGateway(t, content)

	t.Run("gateway only", func(t *testing.T) {
		client := newClient(t, &Config{Gateways: []string{gateway}, DisableDHT: true})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
			t.Fatalf("gateway-only client could not fetch: %v", err)
		}
		if blockRequests.Load() == 0 {
			t.Error("the gateway was never used")
		}
	})

	t.Run("indexer only", func(t *testing.T) {
		indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"Providers":[]}`))
		}))
		t.Cleanup(indexer.Close)

		if _, err := New(&Config{DelegatedRoutingEndpoint: indexer.URL}); err != nil {
			t.Errorf("indexer-only client rejected: %v", err)
		}
	})
}

func TestNewRejectsConfigWithNowhereToFetchFrom(t *testing.T) {
	_, err := New(&Config{})
	if err == nil {
		t.Fatal("expected an error when nothing at all is configured")
	}
	if !strings.Contains(err.Error(), "nowhere to fetch from") {
		t.Errorf("err = %v, want it to say there is nowhere to fetch from", err)
	}
}

// Documented behaviour: a successful download replaces whatever was at output.
func TestGetReplacesExistingOutput(t *testing.T) {
	client, root, content := servedClient(t, 1024)

	output := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(output, []byte("stale content"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("Get over an existing file failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("output was not replaced with the downloaded content")
	}
}

// A bootstrap list that is entirely malformed is a configuration mistake, but it
// is not a reason to refuse a client whose gateways are fine. Where gateways are
// the only route available, failing here would take that away too.
func TestMalformedBootstrapListDoesNotSinkAWorkingClient(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nothing here", http.StatusNotFound)
	}))
	t.Cleanup(gateway.Close)

	client, err := New(&Config{
		BootstrapPeers: []string{"not-a-multiaddr", "/ip4/127.0.0.1/tcp/1"},
		Gateways:       []string{gateway.URL},
	})
	if err != nil {
		t.Fatalf("a client with working gateways was refused: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if len(client.config.peers) != 0 {
		t.Errorf("kept %d peers from a list where none is valid", len(client.config.peers))
	}
}

// With nothing else configured, the same list leaves nowhere to fetch from, and
// that is still an error.
func TestMalformedBootstrapListAloneIsStillRefused(t *testing.T) {
	_, err := New(&Config{BootstrapPeers: []string{"not-a-multiaddr"}})
	if err == nil {
		t.Fatal("a client with no usable route was accepted")
	}
	if !strings.Contains(err.Error(), "nowhere to fetch from") {
		t.Errorf("err = %v, want it to report that there is nowhere to fetch from", err)
	}
}

// The same tolerance the other way round: an unusable gateway list must not take
// away peers that work.
func TestMalformedGatewayListDoesNotSinkAWorkingClient(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client, err := New(&Config{
		BootstrapPeers: []string{addr},
		Gateways:       []string{"not a url", "://also-not"},
	})
	if err != nil {
		t.Fatalf("a client with working peers was refused: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if len(client.config.gateways) != 0 {
		t.Errorf("kept %d gateways from a list where none is valid", len(client.config.gateways))
	}
}
