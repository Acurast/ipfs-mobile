package client

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	routinghttp "github.com/ipfs/boxo/routing/http/client"
	"github.com/ipfs/boxo/routing/http/contentrouter"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
)

const userAgent = "ipfs-mobile"

// Matches boxo's own default cap on a delegated routing response.
const delegatedResponseLimit = 1 << 20

// newDelegatedRouter builds a content router backed by a delegated routing v1
// endpoint, which finds content that is indexed but never announced to the DHT.
func newDelegatedRouter(endpoint string) (routing.ContentDiscovery, error) {
	delegated, err := routinghttp.New(
		endpoint,
		// Wraps boxo's own transport rather than replacing it, so the response-body
		// cap survives: an indexer must not be able to stream unbounded data into a
		// phone. Must precede WithUserAgent, which edits whichever client is set.
		routinghttp.WithHTTPClient(&http.Client{
			Transport: &routinghttp.ResponseBodyLimitedTransport{
				RoundTripper: jsonOnlyTransport{},
				LimitBytes:   delegatedResponseLimit,
			},
		}),
		routinghttp.WithUserAgent(userAgent),
		// Wider than boxo's default, which keeps only bitswap peers. Indexers
		// return HTTP gateways for the same content and the exchange can use them.
		routinghttp.WithProtocolFilter([]string{
			"unknown",
			"transport-bitswap",
			"transport-ipfs-gateway-http",
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("delegated routing endpoint %q: %w", endpoint, err)
	}

	return contentrouter.NewContentRoutingClient(delegated), nil
}

// jsonOnlyTransport forces Accept: application/json on delegated routing
// requests.
//
// boxo prefers to stream and asks for ndjson. cid.contact answers 404 to any
// Accept mentioning ndjson rather than negotiating down, and the routing client
// reads that 404 as "no providers" - so every lookup silently finds nothing.
// Streaming is no loss here; a provider list arrives in one response anyway.
type jsonOnlyTransport struct {
	inner http.RoundTripper
}

func (transport jsonOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// RoundTrip must not modify the request it is handed.
	forwarded := request.Clone(request.Context())
	forwarded.Header.Set("Accept", "application/json")

	inner := transport.inner
	if inner == nil {
		inner = http.DefaultTransport
	}

	return inner.RoundTrip(forwarded)
}

// newProviderFinder composes the configured routers into the one bitswap asks
// for. Nil means no content routing at all: only directly connected peers.
func newProviderFinder(routers ...routing.ContentDiscovery) routing.ContentDiscovery {
	configured := make([]routing.ContentDiscovery, 0, len(routers))
	for _, router := range routers {
		if router != nil {
			configured = append(configured, router)
		}
	}

	switch len(configured) {
	case 0:
		return nil
	case 1:
		return configured[0]
	default:
		return parallelDiscovery(configured)
	}
}

// parallelDiscovery queries every router at once and merges the results. The
// routers know about largely different content and either can be slow, so
// asking in sequence would add one timeout to the other.
type parallelDiscovery []routing.ContentDiscovery

func (routers parallelDiscovery) FindProvidersAsync(ctx context.Context, key cid.Cid, limit int) <-chan peer.AddrInfo {
	merged := make(chan peer.AddrInfo)

	go func() {
		defer close(merged)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var (
			wait  sync.WaitGroup
			mutex sync.Mutex
			seen  = make(map[peer.ID]struct{})
		)

		wait.Add(len(routers))

		for _, router := range routers {
			go func(router routing.ContentDiscovery) {
				defer wait.Done()

				for provider := range router.FindProvidersAsync(ctx, key, limit) {
					mutex.Lock()
					_, duplicate := seen[provider.ID]
					if !duplicate {
						seen[provider.ID] = struct{}{}
					}
					satisfied := limit > 0 && len(seen) >= limit
					mutex.Unlock()

					if duplicate {
						continue
					}

					select {
					case merged <- provider:
					case <-ctx.Done():
						return
					}

					if satisfied {
						// Stop the sibling routers too.
						cancel()
						return
					}
				}
			}(router)
		}

		wait.Wait()
	}()

	return merged
}
