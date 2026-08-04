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

// newDelegatedRouter builds a content router backed by a delegated routing v1
// HTTP endpoint, such as an IPNI indexer.
//
// This exists because the DHT is not the whole picture. Announcing to the DHT
// costs roughly twenty PUTs per CID, republished twice a day, so large pinning
// services publish to an indexer instead. Content indexed only there is
// invisible to a DHT-only client even though public gateways find it easily.
//
// Every lookup tells the endpoint which CID is being fetched, which is why this
// is off unless the caller names an endpoint.
func newDelegatedRouter(endpoint string) (routing.ContentDiscovery, error) {
	delegated, err := routinghttp.New(
		endpoint,
		routinghttp.WithUserAgent(userAgent),
		routinghttp.WithHTTPClient(&http.Client{Transport: jsonOnlyTransport{}}),
		// boxo defaults to {"unknown", "transport-bitswap"}, which throws away
		// gateway records. cid.contact returns both kinds for the same content -
		// a bitswap peer and an HTTP gateway - and since the exchange now speaks
		// HTTP too, discarding the gateway would discard a working route.
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
// boxo asks for "application/x-ndjson,application/json", preferring to stream
// results. cid.contact, which is the indexer most delegated routing endpoints
// front, answers 404 to any Accept that mentions ndjson instead of negotiating
// down to JSON. The routing client reads that 404 as "no providers", so every
// lookup silently returns nothing at all:
//
//	Accept: application/x-ndjson,application/json  ->  404
//	Accept: application/json                       ->  200
//
// Giving up streaming costs nothing here, because a provider list is small
// enough to arrive in one response either way.
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

// newProviderFinder composes the configured routers into the single
// ContentDiscovery that bitswap asks for. A nil result means no content routing
// at all, leaving bitswap able to fetch only from directly connected peers.
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
		// Skip the fan-out machinery when there is nothing to fan out to.
		return configured[0]
	default:
		return parallelDiscovery(configured)
	}
}

// parallelDiscovery queries every router at once and merges the results.
//
// The DHT and an indexer know about largely different content, and either can be
// slow, so asking them in sequence would add one timeout to the other for
// content the first one was never going to find.
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

					// A provider is often known to more than one router.
					if duplicate {
						continue
					}

					select {
					case merged <- provider:
					case <-ctx.Done():
						return
					}

					if satisfied {
						// Enough providers found; stop the sibling routers too.
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
