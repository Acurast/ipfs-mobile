package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SizeLimitError reports content larger than the caller allowed.
//
// A distinct type because it is the one failure no retry can fix - another
// source will not return smaller content - and because the Kotlin wrapper
// selects SizeLimitExceededException on this exact message prefix.
type SizeLimitError struct {
	Size  int64
	Limit int64
}

func (err *SizeLimitError) Error() string {
	return fmt.Sprintf("size limit exceeded (actual size = %d limit = %d bytes)", err.Size, err.Limit)
}

// fetchFromGateways downloads cid over plain HTTP, trying each gateway in turn
// and keeping the first that answers.
//
// This is the last resort, and it is NOT verified. A whole-file gateway response
// cannot be checked against its CID: the CID commits to a DAG - chunk size, leaf
// format, layout - and none of that is recoverable from flat bytes, so identical
// content can legitimately carry different CIDs. Anything fetched here is
// trusted because the gateway said so.
//
// Every other retrieval path in this package verifies. This one exists only
// because some gateways serve files but not blocks, and a caller may prefer
// unverified content to none.
func (client *Client) fetchFromGateways(ctx context.Context, cid string, output string, sizeLimit int64) error {
	if len(client.gateways) == 0 {
		return fmt.Errorf("no gateways configured")
	}

	failures := make([]string, 0, len(client.gateways))

	for _, gateway := range client.gateways {
		err := fetchFromGateway(ctx, gateway, cid, output, sizeLimit)
		if err == nil {
			return nil
		}

		// Pointless to ask another gateway for smaller content.
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

func fetchFromGateway(ctx context.Context, gateway string, cid string, output string, sizeLimit int64) error {
	target, err := url.JoinPath(gateway, "ipfs", cid)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", userAgent)

	response, err := gatewayHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", response.StatusCode)
	}

	// Reject on the advertised length before transferring anything, when the
	// gateway bothers to send one.
	if sizeLimit > 0 && response.ContentLength > sizeLimit {
		return &SizeLimitError{Size: response.ContentLength, Limit: sizeLimit}
	}

	// Staged beside the target and renamed on success, so a failed or truncated
	// transfer never leaves a partial file where the caller expects content.
	scratch, err := os.MkdirTemp(filepath.Dir(output), ".ipfs-gateway-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	staged := filepath.Join(scratch, "data")

	written, err := writeLimited(staged, response.Body, sizeLimit)
	if err != nil {
		return err
	}

	fmt.Printf("fetched %s from gateway %s unverified (%d bytes)\n", cid, gateway, written)

	if err := os.RemoveAll(output); err != nil {
		return err
	}

	return os.Rename(staged, output)
}

// writeLimited streams body to path, stopping if it exceeds sizeLimit. A gateway
// may under-report or omit Content-Length, so the limit is enforced on the bytes
// that actually arrive rather than on what was promised.
func writeLimited(path string, body io.Reader, sizeLimit int64) (int64, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	if sizeLimit <= 0 {
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

// gatewayHTTPClient has no overall timeout: the caller's context bounds the
// request, and a fixed timeout here would cut off a large but healthy download.
var gatewayHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}
