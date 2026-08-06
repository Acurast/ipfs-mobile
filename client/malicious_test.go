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
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"ipfs-mobile/internal/testpeer"
)

// A DAG the CID commits to is richer than a byte string, so a successful Get is
// not by itself evidence that output holds the content. These pin the cases where
// whoever chooses the CID could otherwise decide something else.

// The root is a symlink, so writing the DAG faithfully would leave output
// pointing at a path of the DAG author's choosing - and a caller that then reads
// output reads that file instead, under the caller's own privileges.
func TestRootSymlinkIsRefused(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(secret, []byte("caller's own private data"), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, root := testpeer.ServeSymlink(t, secret)
	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	err := client.Get(ctx, root, output, -1)

	if err == nil {
		t.Fatal("a symlink DAG was accepted")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v, want it to name the symlink", err)
	}

	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Error("output exists after a refused download")
	}
}

// The same, one level down: a directory entry that is a symlink.
func TestSymlinkDirectoryEntryIsRefused(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(secret, []byte("caller's own private data"), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, root := testpeer.ServeDirectoryWithSymlink(t, secret)
	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err == nil {
		t.Fatal("a directory containing a symlink was accepted")
	}

	// Nothing partially written should survive either.
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Error("output exists after a refused download")
	}
}

// The size a DAG declares for itself is metadata nothing reconciles against the
// leaves it links, so the limit has to hold against bytes as they arrive.
func TestUnderdeclaredSizeStillHitsTheLimit(t *testing.T) {
	const (
		declared = 16
		leaves   = 8
		leafSize = 4096
	)

	addr, root := testpeer.ServeUnderdeclaredFile(t, declared, leaves, leafSize)
	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	limit := int64(leaves * leafSize / 4)
	output := filepath.Join(t.TempDir(), "out")
	err := client.Get(ctx, root, output, limit)

	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		t.Fatalf("err = %v, want a SizeLimitError", err)
	}

	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Error("oversized content was written to the output path")
	}
}

// The overflow variant: a declared size that wraps to negative when read as an
// int64 makes any "declared > limit" comparison pass trivially.
func TestOverflowingDeclaredSizeStillHitsTheLimit(t *testing.T) {
	const (
		leaves   = 4
		leafSize = 4096
	)

	addr, root := testpeer.ServeUnderdeclaredFile(t, ^uint64(0), leaves, leafSize)
	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	err := client.Get(ctx, root, output, 1024)

	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		t.Fatalf("err = %v, want a SizeLimitError", err)
	}
}

// An honest DAG of the same shape still arrives, so the limit is enforced rather
// than the walk simply being broken.
func TestUnderdeclaredSizeWithinTheLimitStillArrives(t *testing.T) {
	const (
		leaves   = 4
		leafSize = 1024
	)

	addr, root := testpeer.ServeUnderdeclaredFile(t, 16, leaves, leafSize)
	client := newClient(t, &Config{BootstrapPeers: []string{addr}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, leaves*leafSize); err != nil {
		t.Fatalf("content inside the limit was rejected: %v", err)
	}

	written, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != leaves*leafSize {
		t.Errorf("wrote %d bytes, want %d", len(written), leaves*leafSize)
	}
}

// A string that is not a CID must be refused before anything derives a URL or a
// filesystem path from it - the fallback included.
func TestMalformedCIDTouchesNothing(t *testing.T) {
	var requests atomic.Int32

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-tar")
	}))
	t.Cleanup(gateway.Close)

	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{addr},
		Gateways:                       []string{gateway.URL},
		AllowUnverifiedGatewayFallback: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.db")
	if err := os.WriteFile(victim, []byte("the caller's own data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Shaped the way a caller deriving the output path from the cid would produce.
	for _, malformed := range []string{
		"../victim.db",
		"../../etc/passwd",
		"not-a-cid",
		"",
	} {
		t.Run(malformed, func(t *testing.T) {
			output := filepath.Join(dir, "data", malformed)

			err := client.Get(ctx, malformed, output, -1)
			if err == nil {
				t.Fatal("a malformed cid was accepted")
			}
			if !strings.Contains(err.Error(), "invalid cid") {
				t.Errorf("err = %v, want it to report an invalid cid", err)
			}
		})
	}

	if requests.Load() != 0 {
		t.Errorf("a malformed cid produced %d gateway requests", requests.Load())
	}

	survived, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the caller's file was destroyed: %v", err)
	}
	if !bytes.Equal(survived, []byte("the caller's own data")) {
		t.Error("the caller's file was overwritten")
	}
}

// The gateway URL is built from the canonical CID, so it cannot be steered by the
// string the caller passed.
func TestGatewayURLUsesTheCanonicalCID(t *testing.T) {
	var paths atomic.Value

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.Store(r.URL.Path)
		http.Error(w, "nothing here", http.StatusNotFound)
	}))
	t.Cleanup(gateway.Close)

	addr, _ := testpeer.Serve(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{addr},
		Gateways:                       []string{gateway.URL},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Valid, and served by nobody, so the fallback runs.
	client.Get(ctx, testpeer.UnreachableCID, filepath.Join(t.TempDir(), "out"), -1)

	requested, _ := paths.Load().(string)
	if requested == "" {
		t.Skip("the gateway was not reached; nothing to assert")
	}

	want := "/ipfs/" + mustParseCID(t, testpeer.UnreachableCID).String()
	if requested != want {
		t.Errorf("gateway saw %q, want %q", requested, want)
	}
}

func TestValidEntryName(t *testing.T) {
	refused := []string{"", ".", "..", "a/b", "../escape", "with\x00nul"}
	for _, name := range refused {
		if validEntryName(name) {
			t.Errorf("validEntryName(%q) = true, want false", name)
		}
	}

	for _, name := range []string{"file.txt", "nested-dir", ".hidden", "a.b.c"} {
		if !validEntryName(name) {
			t.Errorf("validEntryName(%q) = false, want true", name)
		}
	}
}

var _ = cid.Cid{}
