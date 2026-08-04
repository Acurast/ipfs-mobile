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
}

// Client is a reusable handle over a running IPFS node. Bootstrap peers are
// dialled on the first download and the connection is reused by later ones.
//
// Bound to Android and iOS by gomobile, so the surface is limited to the types
// it can carry: strings, sized integers and errors.
type Client struct {
	inner *client.Client
}

// NewClient builds a Client. bootstrapPeers is a ";" separated list of
// multiaddrs. A port of 0 picks a free one.
//
// idleTimeout, in milliseconds, closes the underlying node once it has gone
// that long without a download; the next download starts a new one. Zero or
// negative disables it, leaving the node up until Close is called. Either way
// Close must be called when the Client is no longer needed.
func NewClient(bootstrapPeers string, port int32, idleTimeout int64) (*Client, error) {
	inner, err := client.New(&client.Config{
		BootstrapPeers: utils.GetStringSlice(bootstrapPeers),
		Port:           port,
		IdleTimeout:    time.Duration(idleTimeout) * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}

	return &Client{inner: inner}, nil
}

// Get downloads cid to output. A sizeLimit above zero rejects larger content;
// a timeout, in milliseconds, of zero or more bounds the call, and a negative
// one lets it run until it finishes.
func (client *Client) Get(cid string, output string, sizeLimit int64, timeout int64) error {
	ctx, cancel := withTimeout(timeout)
	defer cancel()

	return client.inner.Get(ctx, cid, output, sizeLimit)
}

// Close releases the Client and shuts its node down.
func (client *Client) Close() error {
	return client.inner.Close()
}

// Get downloads a single CID through a Client that is created and closed around
// the call, re-dialling the bootstrap peers every time. Prefer NewClient when
// fetching more than once.
func Get(cid string, output string, config *Config) error {
	client, err := NewClient(config.BootstrapPeers, config.Port, 0)
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
