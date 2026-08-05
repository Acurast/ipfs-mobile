package ffi

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ipfs-mobile/internal/testpeer"
)

func newTestClient(t *testing.T, bootstrapPeers string, idleTimeout int64) *Client {
	t.Helper()

	client, err := NewClient(&ClientConfig{
		BootstrapPeers: bootstrapPeers,
		IdleTimeout:    idleTimeout,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

// A negative timeout means "no timeout", which is how the Kotlin wrapper encodes
// a null Duration. Losing this would turn every unbounded call into an instant
// failure.
func TestWithTimeoutNegativeLeavesTheCallUnbounded(t *testing.T) {
	ctx, cancel := withTimeout(-1)
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("a negative timeout produced a deadline, want none")
	}
}

func TestWithTimeoutNonNegativeSetsADeadline(t *testing.T) {
	tests := []struct {
		name    string
		timeout int64
	}{
		{"zero", 0},
		{"positive", 5000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := withTimeout(test.timeout)
			defer cancel()

			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("timeout %d produced no deadline", test.timeout)
			}

			within := time.Until(deadline)
			want := time.Duration(test.timeout) * time.Millisecond
			if within > want+time.Second {
				t.Errorf("deadline is %v away, want about %v", within, want)
			}
		})
	}
}

func TestNewClientRejectsUnusablePeerLists(t *testing.T) {
	tests := []struct {
		name  string
		peers string
	}{
		{"empty string", ""},
		{"delimiters only", ";;;"},
		{"whitespace", "  ;  "},
		{"all invalid", "not-a-multiaddr;also-not-one"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(&ClientConfig{BootstrapPeers: test.peers})
			if err == nil {
				client.Close()
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// The ";" separated list is how gomobile carries a peer list across the FFI
// boundary, since it cannot bind a []string.
func TestNewClientParsesDelimitedPeerList(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(2048))

	// Padded with the empty entries a joinToString on an list with blanks would
	// produce.
	client := newTestClient(t, ";"+addr+";", 0)

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(root, output, -1, 30000); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestClientGetDownloadsContent(t *testing.T) {
	content := testpeer.Content(4096)
	addr, root := testpeer.Serve(t, content)

	client := newTestClient(t, addr, 0)

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(root, output, -1, 30000); err != nil {
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

func TestClientGetHonoursTimeout(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client := newTestClient(t, addr, 0)

	start := time.Now()
	err := client.Get(testpeer.UnreachableCID, filepath.Join(t.TempDir(), "out"), -1, 500)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout, got nil")
	}

	// The Kotlin wrapper maps anything that is not a size-limit error onto
	// IOException, but the message is what a developer sees in the log.
	if err.Error() != "timeout" {
		t.Errorf("err = %q, want %q", err.Error(), "timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v with a 500ms timeout", elapsed)
	}
}

func TestClientGetEnforcesSizeLimit(t *testing.T) {
	content := testpeer.Content(8192)
	addr, root := testpeer.Serve(t, content)

	client := newTestClient(t, addr, 0)

	err := client.Get(root, filepath.Join(t.TempDir(), "out"), int64(len(content)/2), 30000)
	if err == nil {
		t.Fatal("expected the size limit to be enforced, got nil")
	}

	// SizeLimitExceededException in the Kotlin wrapper is selected on this exact
	// prefix, so changing the wording breaks the consumer's error handling.
	if !strings.HasPrefix(err.Error(), "size limit exceeded") {
		t.Errorf("err = %q, want it to start with %q", err.Error(), "size limit exceeded")
	}
}

func TestClientCloseIsIdempotent(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client, err := NewClient(&ClientConfig{BootstrapPeers: addr})
	if err != nil {
		t.Fatal(err)
	}

	if err := client.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestClientSurvivesIdleShutdown(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	client := newTestClient(t, addr, 100)

	dir := t.TempDir()
	if err := client.Get(root, filepath.Join(dir, "first"), -1, 30000); err != nil {
		t.Fatalf("first download: %v", err)
	}

	// Past the idle window, so the node shuts itself down and the next download
	// has to bring a new one up.
	time.Sleep(400 * time.Millisecond)

	if err := client.Get(root, filepath.Join(dir, "second"), -1, 30000); err != nil {
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

// The package level Get is the one-shot entry point the published binding still
// exposes, so it has to keep working on its own.
func TestGetOneShot(t *testing.T) {
	content := testpeer.Content(2048)
	addr, root := testpeer.Serve(t, content)

	output := filepath.Join(t.TempDir(), "out")
	err := Get(root, output, &Config{
		BootstrapPeers: addr,
		Port:           0,
		SizeLimit:      -1,
		Timeout:        30000,
	})
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

func TestGetOneShotWithoutTimeout(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	output := filepath.Join(t.TempDir(), "out")
	err := Get(root, output, &Config{
		BootstrapPeers: addr,
		SizeLimit:      -1,
		Timeout:        -1,
	})
	if err != nil {
		t.Fatalf("an unbounded Get failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
}

func TestGetOneShotReportsBadConfig(t *testing.T) {
	err := Get("whatever", filepath.Join(t.TempDir(), "out"), &Config{
		BootstrapPeers: "",
		SizeLimit:      -1,
		Timeout:        1000,
	})
	if err == nil {
		t.Fatal("expected an error for an empty peer list, got nil")
	}
}

// The struct carries the options the positional form could not extend, so the
// gateway fields have to reach the client. Timeout plumbing is covered by
// TestMilliseconds and by the client's own tests.
func TestNewClientCarriesGatewayOptions(t *testing.T) {
	content := testpeer.Content(2048)
	gateway, root, blockRequests := testpeer.ServeTrustlessGateway(t, content)

	client, err := NewClient(&ClientConfig{
		Gateways:                       gateway,
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 20000,
		FallbackStepTimeout:            5000,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(root, output, -1, 30000); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if blockRequests.Load() == 0 {
		t.Error("the configured gateway was never used")
	}
}

// A gateway-only client needs no bootstrap peers at all.
func TestNewClientAcceptsGatewaysWithoutPeers(t *testing.T) {
	gateway, _, _ := testpeer.ServeTrustlessGateway(t, testpeer.Content(64))

	client, err := NewClient(&ClientConfig{Gateways: gateway})
	if err != nil {
		t.Fatalf("gateway-only config rejected: %v", err)
	}
	client.Close()
}

func TestMilliseconds(t *testing.T) {
	tests := []struct {
		value int64
		want  time.Duration
	}{
		{-1, 0},
		{0, 0},
		{1500, 1500 * time.Millisecond},
	}

	for _, test := range tests {
		if got := milliseconds(test.value); got != test.want {
			t.Errorf("milliseconds(%d) = %v, want %v", test.value, got, test.want)
		}
	}
}
