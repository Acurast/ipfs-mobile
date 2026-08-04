package ffi

import (
	"context"
	"time"

	"ipfs-mobile/client"
	"ipfs-mobile/utils"
)

// Config configures a one-shot Get.
type Config struct {
	BootstrapPeers string
	Port           int32
	SizeLimit      int64
	Timeout        int64

	// DelegatedRouting is the optional delegated routing v1 endpoint described
	// on NewClient. Empty disables it.
	DelegatedRouting string

	// Gateways and AllowUnverifiedGatewayFallback are as described on NewClient.
	Gateways                       string
	AllowUnverifiedGatewayFallback bool
}

// Client is a reusable handle over a running IPFS node.
//
// Bound to Android and iOS by gomobile, so the surface is limited to the types
// it can carry: strings, sized integers, booleans and errors.
type Client struct {
	inner *client.Client
}

// NewClient builds a Client. bootstrapPeers is a ";" separated list of
// multiaddrs. A port of 0 picks a free one.
//
// idleTimeout, in milliseconds, closes the node once it has gone that long
// without a download; the next one starts a new node. Zero or negative keeps it
// up until Close, which must be called either way.
//
// delegatedRouting is a delegated routing v1 endpoint such as
// "https://cid.contact", queried alongside the DHT. It finds content that is
// indexed but never announced to the DHT; in exchange the endpoint learns which
// CIDs are fetched. Empty disables it.
//
// gateways is a ";" separated list of HTTP gateway URLs, fetched from alongside
// libp2p peers and verified like any other source.
//
// allowUnverifiedGatewayFallback permits a whole-file fetch from those gateways
// once every verified route has failed. That content cannot be checked against
// its CID and is trusted because the gateway served it.
func NewClient(
	bootstrapPeers string,
	port int32,
	idleTimeout int64,
	delegatedRouting string,
	gateways string,
	allowUnverifiedGatewayFallback bool,
) (*Client, error) {
	inner, err := client.New(&client.Config{
		BootstrapPeers:                 utils.GetStringSlice(bootstrapPeers),
		Port:                           port,
		IdleTimeout:                    time.Duration(idleTimeout) * time.Millisecond,
		DelegatedRoutingEndpoint:       delegatedRouting,
		Gateways:                       utils.GetStringSlice(gateways),
		AllowUnverifiedGatewayFallback: allowUnverifiedGatewayFallback,
	})
	if err != nil {
		return nil, err
	}

	return &Client{inner: inner}, nil
}

// Get downloads cid to output. A sizeLimit above zero rejects larger content; a
// timeout in milliseconds bounds the call, and a negative one does not.
func (client *Client) Get(cid string, output string, sizeLimit int64, timeout int64) error {
	ctx, cancel := withTimeout(timeout)
	defer cancel()

	return client.inner.Get(ctx, cid, output, sizeLimit)
}

// Close releases the Client and shuts its node down.
func (client *Client) Close() error {
	return client.inner.Close()
}

// Get downloads a single CID through a Client created and closed around the
// call. Prefer NewClient when fetching more than once.
func Get(cid string, output string, config *Config) error {
	client, err := NewClient(
		config.BootstrapPeers,
		config.Port,
		0,
		config.DelegatedRouting,
		config.Gateways,
		config.AllowUnverifiedGatewayFallback,
	)
	if err != nil {
		return err
	}
	defer client.Close()

	return client.Get(cid, output, config.SizeLimit, config.Timeout)
}

func withTimeout(timeout int64) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		return context.WithCancel(context.Background())
	}

	return context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
}
