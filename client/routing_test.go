package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ipfs-mobile/internal/testpeer"
)

// delegatedRouter stands up a routing v1 endpoint answering provider lookups for
// one CID, in the shape cid.contact returns, and counts the lookups it serves.
func delegatedRouter(t *testing.T, forCID string, provider string) (endpoint string, lookups *atomic.Int32) {
	t.Helper()

	id, addrs := splitPeerAddr(t, provider)
	lookups = &atomic.Int32{}

	mux := http.NewServeMux()
	mux.HandleFunc("/routing/v1/providers/", func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)

		requested := strings.TrimPrefix(r.URL.Path, "/routing/v1/providers/")

		response := map[string]any{"Providers": []any{}}
		if requested == forCID {
			response = map[string]any{
				"Providers": []any{
					map[string]any{
						"Schema":    "peer",
						"ID":        id,
						"Addrs":     addrs,
						"Protocols": []string{"transport-bitswap"},
					},
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server.URL, lookups
}

// splitPeerAddr turns "/ip4/.../tcp/N/p2p/ID" into the ID and the addresses
// without it, which is how a routing v1 record carries them.
func splitPeerAddr(t *testing.T, addr string) (id string, addrs []string) {
	t.Helper()

	const marker = "/p2p/"

	index := strings.LastIndex(addr, marker)
	if index < 0 {
		t.Fatalf("peer address %q has no /p2p/ component", addr)
	}

	return addr[index+len(marker):], []string{addr[:index]}
}

// Content no connected peer holds and the DHT does not know about is still
// retrievable, because the indexer knows who has it.
func TestDelegatedRoutingFindsUnannouncedContent(t *testing.T) {
	content := testpeer.Content(4096)
	bootstrapAddr, providerAddr, root := testpeer.ServeIsolated(t, content)

	endpoint, lookups := delegatedRouter(t, root, providerAddr)

	// No DHT at all, so the indexer is the only way to find the provider.
	client := newClient(t, &Config{
		BootstrapPeers:           []string{bootstrapAddr},
		DisableDHT:               true,
		DelegatedRoutingEndpoint: endpoint,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("delegated routing failed to find the provider: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}

	if lookups.Load() == 0 {
		t.Error("the delegated endpoint was never queried, so something else found the content")
	}
}

// The control for the test above.
func TestWithoutAnyRoutingUnannouncedContentIsUnreachable(t *testing.T) {
	content := testpeer.Content(4096)
	bootstrapAddr, _, root := testpeer.ServeIsolated(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{bootstrapAddr},
		DisableDHT:     true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err == nil {
		t.Fatal("fetched content with no content routing at all")
	}
}

func TestDHTAndDelegatedRoutingRunInParallel(t *testing.T) {
	content := testpeer.Content(2048)
	bootstrapAddr, providerAddr, root := testpeer.ServeIsolated(t, content)

	endpoint, lookups := delegatedRouter(t, root, providerAddr)

	client := newClient(t, &Config{
		BootstrapPeers:           []string{bootstrapAddr},
		DelegatedRoutingEndpoint: endpoint,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("parallel routing failed: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}

	if lookups.Load() == 0 {
		t.Error("the delegated endpoint was never queried")
	}

	// Both routers should be live on the node.
	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node.dht == nil {
		t.Error("dht is not running alongside delegated routing")
	}
}

// Which is why the routers are queried in parallel.
func TestDelegatedRoutingDoesNotBlockTheDHT(t *testing.T) {
	content := testpeer.Content(2048)
	bootstrapAddr, root := testpeer.ServeViaDHT(t, content)

	// Never answers, so anything waiting on it waits for the whole deadline.
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(stalled.Close)

	client := newClient(t, &Config{
		BootstrapPeers:           []string{bootstrapAddr},
		DelegatedRoutingEndpoint: stalled.URL,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("a stalled indexer blocked a DHT lookup that should have worked: %v", err)
	}
	elapsed := time.Since(start)

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}

	t.Logf("fetched via the DHT in %v despite the indexer never answering", elapsed)
}

func TestNewRejectsMalformedDelegatedEndpoint(t *testing.T) {
	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	_, err := New(&Config{
		BootstrapPeers:           []string{addr},
		DelegatedRoutingEndpoint: "://not a url",
	})
	if err == nil {
		t.Fatal("expected a malformed endpoint to be rejected at construction")
	}
	if !strings.Contains(err.Error(), "delegated routing endpoint") {
		t.Errorf("err = %v, want it to name the endpoint", err)
	}
}

func TestDelegatedRoutingDisabledByDefault(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(1024))

	// An endpoint that fails the test if it is ever contacted.
	var contacted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Add(1)
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatal(err)
	}

	if contacted.Load() != 0 {
		t.Error("an endpoint was contacted even though none was configured")
	}
}

func TestNewProviderFinderComposition(t *testing.T) {
	if finder := newProviderFinder(nil, nil); finder != nil {
		t.Errorf("newProviderFinder with no routers = %v, want nil", finder)
	}

	single := parallelDiscovery{}
	if finder := newProviderFinder(single, nil); finder == nil {
		t.Error("newProviderFinder dropped the only configured router")
	}

	both := newProviderFinder(parallelDiscovery{}, parallelDiscovery{})
	if _, ok := both.(parallelDiscovery); !ok {
		t.Errorf("newProviderFinder with two routers = %T, want parallelDiscovery", both)
	}
}
