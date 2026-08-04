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

// SizeLimitError reports content larger than the caller allowed. A distinct
// type because no other source will return smaller content, so it ends the
// retry chain. The Kotlin wrapper selects on this message prefix.
type SizeLimitError struct {
	Size  int64
	Limit int64
}

func (err *SizeLimitError) Error() string {
	return fmt.Sprintf("size limit exceeded (actual size = %d limit = %d bytes)", err.Size, err.Limit)
}

// fetchFromGateways downloads cid over plain HTTP, keeping the first gateway
// that answers.
//
// NOT verified, unlike every other path here. A whole-file response cannot be
// checked against its CID, which commits to a DAG rather than to bytes, so this
// content is trusted because the gateway served it.
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

	// Reject on the advertised length before transferring anything.
	if sizeLimit > 0 && response.ContentLength > sizeLimit {
		return &SizeLimitError{Size: response.ContentLength, Limit: sizeLimit}
	}

	// Staged and renamed, so a failed transfer leaves no partial file at output.
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

// writeLimited streams body to path, stopping if it exceeds sizeLimit. Enforced
// on the bytes that arrive, since Content-Length may be absent or wrong.
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

// No overall timeout: the caller's context bounds the request, and a fixed one
// would cut off a large but healthy download.
var gatewayHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}
