package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ipfs-mobile/internal/testpeer"
)

// The point of the DHT: the client bootstraps to a node that holds no content
// and still fetches, by looking up who provides the CID.
//
// Without content routing this cannot work at all, which is what the disabled
// case below asserts.
func TestDHTFindsContentOnAnUnconnectedProvider(t *testing.T) {
	content := testpeer.Content(4096)
	bootstrapAddr, root := testpeer.ServeViaDHT(t, content)

	client := newClient(t, &Config{BootstrapPeers: []string{bootstrapAddr}})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("dht lookup failed to find the provider: %v", err)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(content))
	}
}

// The mirror image, and the reason the DHT was added: with content routing off,
// the same fetch cannot succeed, because the only connected peer does not hold
// the blocks and there is no way to discover the one that does.
func TestWithoutDHTContentOnAnUnconnectedProviderIsUnreachable(t *testing.T) {
	content := testpeer.Content(4096)
	bootstrapAddr, root := testpeer.ServeViaDHT(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{bootstrapAddr},
		DisableDHT:     true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1)
	if err == nil {
		t.Fatal("fetched content without content routing; the provider should have been undiscoverable")
	}
	if err.Error() != "timeout" {
		t.Errorf("err = %v, want a timeout", err)
	}
}

// A node with the DHT enabled should end up with a usable routing table before
// the download starts, so the first provider lookup has somewhere to ask.
func TestDHTRoutingTableIsReadyBeforeDownloading(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(1024))

	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatal(err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node == nil {
		t.Fatal("node is not running")
	}
	if client.node.dht == nil {
		t.Fatal("dht was not started even though it is enabled")
	}
	if size := client.node.dht.RoutingTable().Size(); size == 0 {
		t.Error("routing table is empty after a successful download")
	}
}

func TestDisableDHTLeavesNoDHTRunning(t *testing.T) {
	addr, root := testpeer.Serve(t, testpeer.Content(1024))

	client := newClient(t, &Config{
		BootstrapPeers: []string{addr},
		DisableDHT:     true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Still fetches: the bootstrap peer holds the content and is directly
	// connected, so no routing is needed.
	if err := client.Get(ctx, root, filepath.Join(t.TempDir(), "out"), -1); err != nil {
		t.Fatalf("direct fetch from the connected peer failed: %v", err)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	if client.node.dht != nil {
		t.Error("dht was started even though DisableDHT is set")
	}
}

// Starting and stopping the node repeatedly must not leak or wedge the DHT.
func TestDHTSurvivesNodeRestart(t *testing.T) {
	content := testpeer.Content(1024)
	addr, root := testpeer.Serve(t, content)

	client := newClient(t, &Config{
		BootstrapPeers: []string{addr},
		IdleTimeout:    150 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()
	for i := range 3 {
		if err := client.Get(ctx, root, filepath.Join(dir, "out"), -1); err != nil {
			t.Fatalf("download %d: %v", i, err)
		}

		got, err := os.ReadFile(filepath.Join(dir, "out"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("download %d: content mismatch", i)
		}

		// Let the idle timer tear the node, and its DHT, down.
		deadline := time.Now().Add(10 * time.Second)
		for {
			client.mutex.Lock()
			down := client.node == nil
			client.mutex.Unlock()

			if down {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("download %d: node never shut down", i)
			}

			time.Sleep(20 * time.Millisecond)
		}
	}
}
