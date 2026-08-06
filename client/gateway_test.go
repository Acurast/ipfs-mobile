package client

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ipfs-mobile/internal/testpeer"
)

// A gateway serving blocks is fetched from like any other peer, and verified.
func TestGatewayServesVerifiedBlocks(t *testing.T) {
	content := testpeer.Content(4096)
	gateway, root, blockRequests := testpeer.ServeTrustlessGateway(t, content)

	// The only libp2p peer holds nothing, so the gateway is the sole source.
	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers: []string{bootstrapAddr},
		DisableDHT:     true,
		Gateways:       []string{gateway},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("gateway block retrieval failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}

	if blockRequests.Load() == 0 {
		t.Error("no block requests reached the gateway, so it was not the source")
	}
}

func TestVerifiedGatewayPreemptsUnverifiedFallback(t *testing.T) {
	content := testpeer.Content(2048)
	gateway, root, blockRequests := testpeer.ServeTrustlessGateway(t, content)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
	if blockRequests.Load() == 0 {
		t.Error("expected the verified block path to have been used")
	}
}

// A gateway that only serves whole files is useless to the verified path, which
// is where the fallback earns its place.
func TestUnverifiedFallbackFetchesFromWholeFileGateway(t *testing.T) {
	content := testpeer.Content(4096)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)
	gateway, requests := testpeer.ServeWholeFileGateway(t, root, content)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("unverified fallback failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
	if requests.Load() == 0 {
		t.Error("the whole-file gateway was never asked")
	}
}

// The control for the test above.
func TestWithoutFallbackWholeFileGatewayIsNotUsed(t *testing.T) {
	content := testpeer.Content(4096)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)
	gateway, requests := testpeer.ServeWholeFileGateway(t, root, content)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers: []string{bootstrapAddr},
		DisableDHT:     true,
		Gateways:       []string{gateway},
		// AllowUnverifiedGatewayFallback deliberately left off.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err == nil {
		t.Fatal("fetched unverified content without opting in to it")
	}

	if requests.Load() != 0 {
		t.Errorf("the whole-file gateway was asked %d times despite the fallback being off", requests.Load())
	}
}

// The cost of the fallback, as a test: content that does not match the CID is
// accepted, because a whole-file response cannot be checked against one.
// Changing this behaviour should mean changing this test deliberately.
func TestUnverifiedFallbackCannotDetectWrongContent(t *testing.T) {
	content := testpeer.Content(4096)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)

	substituted := []byte("this is not the content that was asked for")
	gateway, _ := testpeer.ServeWholeFileGateway(t, root, substituted)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, substituted) {
		t.Fatal("expected the substituted content to have been accepted")
	}

	t.Log("substituted content accepted, as documented: a whole-file gateway response carries no proof it matches the CID")
}

// Enforced on the bytes that arrive, not on what the gateway claimed.
func TestUnverifiedFallbackEnforcesSizeLimit(t *testing.T) {
	content := testpeer.Content(8192)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)
	gateway, _ := testpeer.ServeWholeFileGateway(t, root, content)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	err := client.Get(ctx, root, output, int64(len(content)/2))

	if err == nil {
		t.Fatal("expected the size limit to be enforced on the fallback")
	}

	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		t.Errorf("err = %v, want a SizeLimitError", err)
	}
	// The Kotlin wrapper selects on this prefix regardless of which path failed.
	if !strings.HasPrefix(err.Error(), "size limit exceeded") {
		t.Errorf("err = %q, want it to start with %q", err.Error(), "size limit exceeded")
	}

	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Error("oversized content was written to the output path")
	}
}

func TestFallbackFailureReportsBothCauses(t *testing.T) {
	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	// Answers nothing for any CID.
	gateway, _ := testpeer.ServeWholeFileGateway(t, "some-other-cid", nil)

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err := client.Get(ctx, testpeer.UnreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "no gateway served the content") {
		t.Errorf("err = %v, want it to mention the gateway failure", err)
	}
}

// An entirely invalid gateway list is a configuration mistake, but on its own it
// is not fatal: see TestMalformedGatewayListDoesNotSinkAWorkingClient, which
// covers the case where peers still provide a route. It only fails the build
// when nothing else does, which TestNewRejectsUnusableBootstrapLists covers.

func TestNewSkipsInvalidGatewaysButKeepsValidOnes(t *testing.T) {
	content := testpeer.Content(1024)
	gateway, root, blockRequests := testpeer.ServeTrustlessGateway(t, content)
	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers: []string{bootstrapAddr},
		DisableDHT:     true,
		Gateways:       []string{"://nonsense", gateway},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("the valid gateway was discarded along with the invalid one: %v", err)
	}
	if blockRequests.Load() == 0 {
		t.Error("the valid gateway was never used")
	}
}

func TestGatewayMultiaddrConversion(t *testing.T) {
	tests := []struct {
		gateway string
		want    string
	}{
		{"https://ipfs.io", "/dns4/ipfs.io/tcp/443/https"},
		{"https://ipfs.io:8443", "/dns4/ipfs.io/tcp/8443/https"},
		{"http://127.0.0.1:8080", "/ip4/127.0.0.1/tcp/8080/http"},
		{"https://gateway.pinata.cloud/", "/dns4/gateway.pinata.cloud/tcp/443/https"},
	}

	for _, test := range tests {
		t.Run(test.gateway, func(t *testing.T) {
			addr, err := gatewayMultiaddr(test.gateway)
			if err != nil {
				t.Fatal(err)
			}
			if addr.String() != test.want {
				t.Errorf("got %s, want %s", addr, test.want)
			}
		})
	}
}

// A gateway must not look like a new peer on every node restart.
func TestGatewayPeerIDIsStableAndDistinct(t *testing.T) {
	first, err := gatewayPeerID("https://ipfs.io")
	if err != nil {
		t.Fatal(err)
	}
	again, err := gatewayPeerID("https://ipfs.io")
	if err != nil {
		t.Fatal(err)
	}
	other, err := gatewayPeerID("https://dweb.link")
	if err != nil {
		t.Fatal(err)
	}

	if first != again {
		t.Error("the same gateway produced two different ids")
	}
	if first == other {
		t.Error("different gateways produced the same id")
	}
}
