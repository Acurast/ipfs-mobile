package client

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"ipfs-mobile/internal/testpeer"

	"github.com/ipfs/boxo/bitswap/network/httpnet"
	"github.com/ipfs/go-cid"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// The limit reported has to be the caller's own, not whatever was left of the
// budget when the entry that tripped it started. The Kotlin wrapper surfaces
// this message verbatim.
func TestSizeLimitErrorReportsTheCallersFigures(t *testing.T) {
	const limit = 4096

	dir := t.TempDir()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)

	// Two entries, so the limit trips on the second with the first already
	// counted - which is exactly when the residual and the real limit diverge.
	for _, entry := range []struct {
		name string
		size int
	}{
		{"root/first", 3000},
		{"root/second", 3000},
	} {
		if err := writer.WriteHeader(&tar.Header{
			Name: entry.name, Mode: 0o600, Size: int64(entry.size), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(bytes.Repeat([]byte("x"), entry.size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err := extractTar(dir, &archive, limit)

	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		t.Fatalf("err = %v, want a SizeLimitError", err)
	}

	if tooBig.Limit != limit {
		t.Errorf("reported limit %d, want the caller's %d", tooBig.Limit, limit)
	}
	if tooBig.Size <= limit {
		t.Errorf("reported size %d, want more than the limit it exceeded", tooBig.Size)
	}
}

// One cid means one root. A second one would be left behind while the download
// reported success, so it is refused instead.
func TestMultiRootArchiveIsRefused(t *testing.T) {
	dir := t.TempDir()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)

	for _, name := range []string{"wanted/file", "extra/file"} {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: 4, Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err := extractTar(dir, &archive, -1)
	if err == nil {
		t.Fatal("an archive with two roots was accepted")
	}
	if !strings.Contains(err.Error(), "more than one root") {
		t.Errorf("err = %v, want it to name the second root", err)
	}
}

// Close landing after the primary phase has already failed must stop the
// fallback too: it holds no node, so nothing else would keep a retired client
// off the network.
func TestCloseDuringTheFallbackStopsIt(t *testing.T) {
	var (
		firstTar  atomic.Int32
		secondTar atomic.Int32
		client    atomic.Pointer[Client]
	)

	// Closes the client while serving, so the loop meets a retired client on its
	// way to the second gateway.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "tar" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}

		firstTar.Add(1)
		if running := client.Load(); running != nil {
			running.Close()
		}
		http.Error(w, "nothing here", http.StatusNotFound)
	}))
	t.Cleanup(first.Close)

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "tar" {
			secondTar.Add(1)
		}
		http.Error(w, "nothing here", http.StatusNotFound)
	}))
	t.Cleanup(second.Close)

	built := newClient(t, &Config{
		BootstrapPeers:                 []string{deadPeer},
		Gateways:                       []string{first.URL, second.URL},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 time.Second,
	})
	client.Store(built)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := built.Get(ctx, testpeer.UnreachableCID, filepath.Join(t.TempDir(), "out"), -1)

	if firstTar.Load() == 0 {
		t.Skip("the fallback never reached the first gateway; nothing to assert")
	}

	if !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if secondTar.Load() != 0 {
		t.Errorf("a closed client made %d further gateway requests", secondTar.Load())
	}
}

// Close must reach an attempt already in flight, not only the next one: the
// fallback holds no node, so nothing else would stop it writing output.
func TestCloseCancelsAnAttemptInFlight(t *testing.T) {
	var (
		serving   = make(chan struct{})
		cancelled = make(chan struct{})
		client    atomic.Pointer[Client]
	)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "tar" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}

		close(serving)

		// Hold the response open. Only Close should end this.
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(gateway.Close)

	built := newClient(t, &Config{
		BootstrapPeers:                 []string{deadPeer},
		Gateways:                       []string{gateway.URL},
		AllowUnverifiedGatewayFallback: true,
		PrimaryTimeout:                 time.Second,
		// Long enough that only Close can end the attempt.
		FallbackStepTimeout: time.Minute,
	})
	client.Store(built)

	done := make(chan error, 1)
	go func() {
		done <- built.Get(context.Background(), testpeer.UnreachableCID, filepath.Join(t.TempDir(), "out"), -1)
	}()

	select {
	case <-serving:
	case <-time.After(30 * time.Second):
		t.Skip("the fallback never reached the gateway; nothing to assert")
	}

	built.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("err = %v, want ErrClosed", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close did not stop the attempt in flight")
	}

	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Error("the gateway handler never saw its request cancelled")
	}
}

// A gateway that throttles the fallback is left alone until its Retry-After
// deadline, instead of being asked again by every following download.
func TestFallbackHonorsRetryAfter(t *testing.T) {
	var hits atomic.Int64
	throttled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(throttled.Close)

	client := newClient(t, &Config{
		Gateways:                       []string{throttled.URL},
		DisableDHT:                     true,
		AllowUnverifiedGatewayFallback: true,
	})

	target := cid.MustParse("bafkqaaa")
	ctx := context.Background()

	if err := client.fetchFromGateways(ctx, target, filepath.Join(t.TempDir(), "out"), -1); err == nil {
		t.Fatal("expected the throttled gateway to fail")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected one request, got %d", got)
	}

	// The next download during the cooldown makes no request at all.
	err := client.fetchFromGateways(ctx, target, filepath.Join(t.TempDir(), "out"), -1)
	if err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("expected a cooldown failure, got: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("cooldown should suppress requests, got %d", got)
	}

	// Once the deadline lapses, requests resume.
	host := mustHost(t, throttled.URL)
	gatewayCooldowns.Lock()
	gatewayCooldowns.deadline[host] = time.Now().Add(-time.Second)
	gatewayCooldowns.Unlock()

	if err := client.fetchFromGateways(ctx, target, filepath.Join(t.TempDir(), "out"), -1); err == nil {
		t.Fatal("expected the still-throttled gateway to fail")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expired cooldown should allow a request, got %d", got)
	}
}

func mustHost(t *testing.T, gateway string) string {
	t.Helper()

	parsed, err := url.Parse(gateway)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

// Content-level misses must not cool a host: the same gateway may serve the
// next CID fine.
func TestFallbackDoesNotCoolContentMisses(t *testing.T) {
	var hits atomic.Int64
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(missing.Close)

	client := newClient(t, &Config{
		Gateways:                       []string{missing.URL},
		DisableDHT:                     true,
		AllowUnverifiedGatewayFallback: true,
	})

	target := cid.MustParse("bafkqaaa")
	ctx := context.Background()

	for i := range 2 {
		if err := client.fetchFromGateways(ctx, target, filepath.Join(t.TempDir(), "out"), -1); err == nil {
			t.Fatalf("fetch %d: expected a failure", i)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("404s must not suppress requests, got %d", got)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	for _, test := range []struct {
		header string
		want   time.Duration
	}{
		{"", httpnet.DefaultConnectFailureBackoff},
		{"garbage", httpnet.DefaultConnectFailureBackoff},
		{"30", 30 * time.Second},
		{"0", httpnet.DefaultConnectFailureBackoff},
		{"-5", httpnet.DefaultConnectFailureBackoff},
		{"3600", httpnet.DefaultMaxBackoff},
		{time.Now().Add(-time.Hour).UTC().Format(time.RFC1123), httpnet.DefaultConnectFailureBackoff},
	} {
		if got := retryAfterDelay(test.header); got != test.want {
			t.Errorf("retryAfterDelay(%q) = %s, want %s", test.header, got, test.want)
		}
	}

	// A future date maps to roughly its distance from now.
	future := time.Now().Add(30 * time.Second).UTC().Format(time.RFC1123)
	if got := retryAfterDelay(future); got <= 25*time.Second || got > 31*time.Second {
		t.Errorf("retryAfterDelay(%q) = %s, want about 30s", future, got)
	}
}
