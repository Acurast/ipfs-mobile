package client

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
)

const gatewayScratchPrefix = ".ipfs-gateway-"

// SizeLimitError reports content larger than the caller allowed. A distinct
// type because no other source will return smaller content, so it ends the
// retry chain. The Kotlin wrapper selects on this message prefix.
type SizeLimitError struct {
	Size  int64
	Limit int64
}

// sizeLimitWithTotals restates a size limit error in the caller's terms, since
// writeLimited only ever sees its own share of the budget.
func sizeLimitWithTotals(err error, written int64, sizeLimit int64) error {
	var tooBig *SizeLimitError
	if !errors.As(err, &tooBig) {
		return err
	}

	return &SizeLimitError{Size: written, Limit: sizeLimit}
}

func (err *SizeLimitError) Error() string {
	return fmt.Sprintf("size limit exceeded (actual size = %d limit = %d bytes)", err.Size, err.Limit)
}

// fetchFromGateways downloads cid over plain HTTP, keeping the first gateway
// that answers.
//
// NOT verified, unlike every other path here. A gateway response cannot be
// checked against its CID, which commits to a DAG rather than to bytes, so this
// content is trusted because the gateway served it.
func (client *Client) fetchFromGateways(ctx context.Context, target cid.Cid, output string, sizeLimit int64) error {
	if len(client.gateways) == 0 {
		return fmt.Errorf("no gateways configured")
	}

	failures := make([]string, 0, len(client.gateways))

	for _, gateway := range client.gateways {
		// Re-checked per attempt rather than once: a Close landing mid-run should
		// cost at most the attempt already in flight.
		if client.isClosed() {
			return ErrClosed
		}

		attempt, cancel := context.WithTimeout(ctx, client.fallbackStepTimeout)
		stopWatching := context.AfterFunc(client.retired, cancel)

		err := fetchFromGateway(attempt, gateway, target, output, sizeLimit)

		stopWatching()
		cancel()

		if err == nil {
			return nil
		}

		// Cancelled by Close rather than by the caller or the step budget.
		if client.isClosed() {
			return ErrClosed
		}

		// No other gateway returns smaller content.
		if limit, ok := err.(*SizeLimitError); ok {
			return limit
		}

		if ctx.Err() != nil {
			return contextError(ctx)
		}

		failures = append(failures, fmt.Sprintf("%s: %s", gateway, err))
	}

	return fmt.Errorf("no gateway served the content (%s)", strings.Join(failures, "; "))
}

// fetchFromGateway asks for the content as a tar archive rather than as a plain
// body.
//
// A plain GET of a directory CID returns the gateway's HTML index page, which
// would be written out as though it were the content. Tar carries the file or
// directory tree itself, so what lands at output has the right shape.
func fetchFromGateway(ctx context.Context, gateway string, target cid.Cid, output string, sizeLimit int64) error {
	// The canonical CID string, never the caller's: it is about to become a URL
	// path segment.
	endpoint, err := url.JoinPath(gateway, "ipfs", target.String())
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?format=tar", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/x-tar")
	request.Header.Set("User-Agent", userAgent)

	response, err := gatewayHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", response.StatusCode)
	}

	// Refuse anything that is not the archive that was asked for, so an HTML error
	// page or index cannot be mistaken for content.
	if mediaType := response.Header.Get("Content-Type"); mediaType != "" && !strings.HasPrefix(mediaType, "application/x-tar") {
		return fmt.Errorf("gateway served %q, want application/x-tar", mediaType)
	}

	sweepScratchPrefix(filepath.Dir(output), gatewayScratchPrefix)

	scratch, release, err := claimScratch(filepath.Dir(output), gatewayScratchPrefix)
	if err != nil {
		return err
	}
	defer release()

	staged, written, err := extractTar(scratch, response.Body, sizeLimit)
	if err != nil {
		return err
	}

	fmt.Printf("fetched %s from gateway %s unverified (%d bytes)\n", target, gateway, written)

	return replace(ctx, staged, output)
}

// extractTar unpacks the archive into dir and returns the path of its single
// root entry, which is what gets moved into place.
//
// The archive comes from a source this path does not trust, so entries that
// escape dir, and anything that is not a plain file or directory, are refused
// rather than skipped: a gateway with no reason to send them is misbehaving.
func extractTar(dir string, body io.Reader, sizeLimit int64) (root string, written int64, err error) {
	archive := tar.NewReader(body)

	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", written, err
		}

		path, top, err := resolveTarPath(dir, header.Name)
		if err != nil {
			return "", written, err
		}

		// The top level component, not the first entry: entries arrive in whatever
		// order the archive lists them, so the first one may be a nested file.
		switch {
		case root == "":
			root = top
		case top != root:
			// One cid, one root. Only the root is moved into place, so a second would
			// be dropped and the download still called a success.
			return "", written, fmt.Errorf(
				"gateway served an archive with more than one root entry (%q and %q)", root, top,
			)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return "", written, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", written, err
			}

			remaining := int64(-1)
			if sizeLimit > 0 {
				remaining = sizeLimit - written
			}

			count, err := writeLimited(path, archive, remaining)
			written += count
			if err != nil {
				return "", written, sizeLimitWithTotals(err, written, sizeLimit)
			}
		default:
			return "", written, fmt.Errorf("unsupported tar entry %q of type %d", header.Name, header.Typeflag)
		}
	}

	if root == "" {
		return "", written, fmt.Errorf("gateway served an empty archive")
	}

	return root, written, nil
}

// resolveTarPath places a tar entry under dir, refusing any name that would
// escape it, and reports the top level path the entry sits beneath.
func resolveTarPath(dir string, name string) (path string, top string, err error) {
	if name == "" {
		return "", "", fmt.Errorf("tar entry has no name")
	}

	// Rooting the name before cleaning collapses any ".." it contains, so the
	// result cannot climb above dir.
	cleaned := filepath.Clean("/" + filepath.FromSlash(name))
	path = filepath.Join(dir, cleaned)

	if path != dir && !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("tar entry %q escapes the staging directory", name)
	}

	elements := strings.Split(strings.TrimPrefix(filepath.ToSlash(cleaned), "/"), "/")
	if len(elements) == 0 || elements[0] == "" {
		return "", "", fmt.Errorf("tar entry %q has no top level name", name)
	}

	return path, filepath.Join(dir, elements[0]), nil
}

// writeLimited streams body to path, stopping if it exceeds sizeLimit. Enforced
// on the bytes that arrive, since Content-Length may be absent or wrong. A
// negative limit means unlimited.
func writeLimited(path string, body io.Reader, sizeLimit int64) (int64, error) {
	// O_EXCL so an existing entry - including a symlink planted by an earlier
	// entry in the same archive - is never opened or followed.
	file, err := os.OpenFile(path, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	if sizeLimit < 0 {
		return io.Copy(file, body)
	}

	// One byte past the limit is enough to know it was exceeded.
	written, err := io.Copy(file, io.LimitReader(body, sizeLimit+1))
	if err != nil {
		return written, err
	}

	if written > sizeLimit {
		return written, &SizeLimitError{Size: written, Limit: sizeLimit}
	}

	return written, nil
}

// No overall timeout: Config.FallbackStepTimeout bounds each attempt, and a fixed
// one here would cut off a large but healthy download.
var gatewayHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}
