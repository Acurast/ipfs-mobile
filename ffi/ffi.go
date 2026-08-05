package ffi

import (
	"context"
	"time"

	"ipfs-mobile/client"
	"ipfs-mobile/utils"
)

// ClientConfig configures a reusable Client.
//
// A struct rather than parameters because gomobile can only carry strings, sized
// integers and booleans, so every option would otherwise be another positional
// argument at the call site.
//
// bootstrapPeers and gateways are ";" separated lists. Durations are in
// milliseconds, and zero or negative selects the documented default.
type ClientConfig struct {
	BootstrapPeers string
	Port           int32
	Gateways       string

	// DelegatedRouting is a delegated routing v1 endpoint such as
	// "https://cid.contact", queried alongside the DHT. It finds content that is
	// indexed but never announced to the DHT; in exchange the endpoint learns
	// which CIDs are fetched. Empty disables it.
	DelegatedRouting string

	// IdleTimeout closes the node once it has gone that long without a download;
	// the next one starts a new node. Zero or negative keeps it up until Close,
	// which must be called either way.
	IdleTimeout int64

	// AllowUnverifiedGatewayFallback permits a gateway fetch once every verified
	// route has failed. That content cannot be checked against its CID and is
	// trusted because the gateway served it.
	AllowUnverifiedGatewayFallback bool

	// PrimaryTimeout is the most the primary phase may take before the fallback
	// above is allowed to start. A download's own timeout shortens it but cannot
	// extend it, so some of that timeout always survives for the fallback.
	PrimaryTimeout int64

	// FallbackStepTimeout is the most any single gateway attempt may take. Per
	// attempt rather than per phase, so dead gateways cannot starve the one that
	// would have answered.
	FallbackStepTimeout int64
}

// Config configures a one-shot Get.
type Config struct {
	BootstrapPeers string
	Port           int32
	SizeLimit      int64
	Timeout        int64

	// As described on ClientConfig.
	DelegatedRouting               string
	Gateways                       string
	AllowUnverifiedGatewayFallback bool
	PrimaryTimeout                 int64
	FallbackStepTimeout            int64
}

func (config *Config) clientConfig() *ClientConfig {
	return &ClientConfig{
		BootstrapPeers:                 config.BootstrapPeers,
		Port:                           config.Port,
		Gateways:                       config.Gateways,
		DelegatedRouting:               config.DelegatedRouting,
		AllowUnverifiedGatewayFallback: config.AllowUnverifiedGatewayFallback,
		PrimaryTimeout:                 config.PrimaryTimeout,
		FallbackStepTimeout:            config.FallbackStepTimeout,
	}
}

// Client is a reusable handle over a running IPFS node.
//
// Bound to Android and iOS by gomobile, so the surface is limited to the types
// it can carry: strings, sized integers, booleans and errors.
type Client struct {
	inner *client.Client
}

// NewClient builds a Client. Close must be called when it is no longer needed.
func NewClient(config *ClientConfig) (*Client, error) {
	inner, err := client.New(&client.Config{
		BootstrapPeers:                 utils.GetStringSlice(config.BootstrapPeers),
		Port:                           config.Port,
		Gateways:                       utils.GetStringSlice(config.Gateways),
		DelegatedRoutingEndpoint:       config.DelegatedRouting,
		IdleTimeout:                    milliseconds(config.IdleTimeout),
		AllowUnverifiedGatewayFallback: config.AllowUnverifiedGatewayFallback,
		PrimaryTimeout:                 milliseconds(config.PrimaryTimeout),
		FallbackStepTimeout:            milliseconds(config.FallbackStepTimeout),
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
	client, err := NewClient(config.clientConfig())
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

// milliseconds converts an FFI duration, leaving a non-positive value alone so
// the client applies its own default.
func milliseconds(value int64) time.Duration {
	if value <= 0 {
		return 0
	}

	return time.Duration(value) * time.Millisecond
}
