package client

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"ipfs-mobile/internal/testpeer"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A directory CID must not come back as the gateway's HTML index page.
func TestUnverifiedFallbackRejectsHTMLIndex(t *testing.T) {
	content := testpeer.Content(1024)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!DOCTYPE html><html><body>directory listing</body></html>"))
	}))
	t.Cleanup(html.Close)

	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{html.URL},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err == nil {
		t.Fatal("accepted an HTML index page as content")
	}

	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Error("HTML was written to the output path")
	}
}

// The fallback asks for a tar and unpacks it, so a directory keeps its shape.
func TestUnverifiedFallbackRestoresDirectories(t *testing.T) {
	tree := map[string][]byte{
		"root/one.txt":       []byte("first"),
		"root/nested/two.md": []byte("second"),
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		writeTar(t, w, tree)
	}))
	t.Cleanup(gateway.Close)

	content := testpeer.Content(1024)
	_, root, _ := testpeer.ServeTrustlessGateway(t, content)
	bootstrapAddr, _, _ := testpeer.ServeIsolated(t, testpeer.Content(64))

	client := newClient(t, &Config{
		BootstrapPeers:                 []string{bootstrapAddr},
		DisableDHT:                     true,
		Gateways:                       []string{gateway.URL},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	output := filepath.Join(t.TempDir(), "out")
	if err := client.Get(ctx, root, output, -1); err != nil {
		t.Fatalf("tar fallback failed: %v", err)
	}

	for name, want := range map[string][]byte{
		"one.txt":       []byte("first"),
		"nested/two.md": []byte("second"),
	} {
		got, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The archive comes from an untrusted source, so an entry that climbs out of the
// staging directory must be refused rather than followed.
func TestExtractTarRefusesEscapingEntries(t *testing.T) {
	for _, name := range []string{"../escaped", "root/../../escaped", "/absolute"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			staging := filepath.Join(dir, "staging")
			if err := os.Mkdir(staging, 0o755); err != nil {
				t.Fatal(err)
			}

			var archive bytes.Buffer
			writeTar(t, &archive, map[string][]byte{name: []byte("payload")})

			_, _, err := extractTar(staging, &archive, -1)

			// Either refused outright, or contained: what must not happen is a file
			// appearing outside the staging directory.
			if err == nil {
				escaped := filepath.Join(dir, "escaped")
				if _, statErr := os.Stat(escaped); statErr == nil {
					t.Fatalf("entry %q was written outside the staging directory", name)
				}
			}
		})
	}
}

func TestExtractTarRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "root/link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	if _, _, err := extractTar(dir, &archive, -1); err == nil {
		t.Error("a symlink entry was accepted")
	}
}

func TestExtractTarEnforcesSizeLimitAcrossEntries(t *testing.T) {
	var archive bytes.Buffer
	writeTar(t, &archive, map[string][]byte{
		"root/a": bytes.Repeat([]byte("a"), 600),
		"root/b": bytes.Repeat([]byte("b"), 600),
	})

	_, _, err := extractTar(t.TempDir(), &archive, 1000)

	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		t.Errorf("err = %v, want a SizeLimitError once the total exceeded the limit", err)
	}
}

func writeTar(t *testing.T, into interface{ Write([]byte) (int, error) }, entries map[string][]byte) {
	t.Helper()

	writer := tar.NewWriter(into)
	defer writer.Close()

	for name, body := range entries {
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
}
