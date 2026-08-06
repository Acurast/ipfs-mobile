package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	"ipfs-mobile/utils"
)

// ErrClosed is returned by Get once the Client has been closed.
var ErrClosed = errors.New("client is closed")

const (
	defaultPrimaryTimeout      = 15 * time.Second
	defaultFallbackStepTimeout = 15 * time.Second
)

type Config struct {
	BootstrapPeers []string
	Port           int32

	// IdleTimeout closes the node once it has gone this long without a download;
	// the next one starts a new node. Zero or negative keeps it up until Close.
	IdleTimeout time.Duration

	// DisableDHT turns off DHT provider lookups, leaving only directly connected
	// peers and whatever DelegatedRoutingEndpoint finds.
	DisableDHT bool

	// DelegatedRoutingEndpoint resolves providers through a delegated routing v1
	// endpoint such as "https://cid.contact", alongside the DHT. Empty disables it.
	//
	// Large pinning services publish to an indexer instead of announcing every CID
	// to the DHT, so some content is findable no other way. In exchange the
	// endpoint learns which CIDs are fetched, and by whom.
	DelegatedRoutingEndpoint string

	// DNSServers are the resolvers used to look up addresses, for example
	// "1.1.1.1" or "2606:4700:4700::1111". A port may be given, and is 53 without
	// one.
	//
	// Only /dnsaddr/ bootstrap addresses need this, and only on platforms that
	// publish no resolver configuration of their own - Android among them, where
	// they are otherwise unresolvable. Empty uses whatever the platform offers.
	DNSServers []string

	// Gateways are HTTP gateway URLs, for example "https://ipfs.io", fetched from
	// alongside libp2p peers. What they return is verified like any other block.
	Gateways []string

	// PrimaryTimeout is the most the primary phase - peers and gateways, every
	// block verified - may take before the fallback is allowed to start. Zero or
	// negative uses 15s.
	//
	// A caller deadline shortens it but cannot extend it, so some of that deadline
	// always survives for the fallback to use.
	PrimaryTimeout time.Duration

	// FallbackStepTimeout is the most any single gateway attempt in the fallback
	// may take. Zero or negative uses 15s.
	//
	// Per attempt rather than per phase, so a run of dead gateways cannot starve
	// the one that would have answered. With no caller deadline the fallback can
	// therefore run for this long once per configured gateway.
	FallbackStepTimeout time.Duration

	// AllowUnverifiedGatewayFallback permits a whole-file fetch from Gateways once
	// every verified route has failed.
	//
	// Such a response cannot be checked against its CID, which commits to a DAG
	// rather than to bytes, so the content is trusted purely because the gateway
	// served it. Enable it when unverified content still beats none.
	AllowUnverifiedGatewayFallback bool
}

// Client is a reusable handle over an IPFS node, started on the first download
// and reused by later ones. Safe for concurrent use; Close releases it.
type Client struct {
	config nodeConfig
	idle   time.Duration

	gateways            []string
	allowUnverified     bool
	primaryTimeout      time.Duration
	fallbackStepTimeout time.Duration

	mutex    sync.Mutex
	node     *node
	inflight int
	timer    *time.Timer
	closed   bool
}

// New validates the configuration and returns a Client. The node itself is not
// started until the first download.
func New(config *Config) (*Client, error) {
	peers := parsePeers(config.BootstrapPeers)

	gateways, gatewayHosts := parseGateways(config.Gateways)

	resolver, err := newResolver(config.DNSServers)
	if err != nil {
		return nil, err
	}

	node := nodeConfig{
		port:         config.Port,
		peers:        peers,
		disableDHT:   config.DisableDHT,
		gateways:     gateways,
		gatewayHosts: gatewayHosts,
		resolver:     resolver,
	}

	if config.DelegatedRoutingEndpoint != "" {
		delegated, err := newDelegatedRouter(config.DelegatedRoutingEndpoint)
		if err != nil {
			return nil, err
		}

		node.delegated = delegated
	}

	// Peers, gateways and an indexer are each enough on their own, so the
	// requirement is only that one of them survived parsing. Invalid entries are
	// skipped rather than fatal, since a typo in one list should not take away a
	// route that another list still provides.
	if len(peers) == 0 && len(gateways) == 0 && node.delegated == nil {
		if configured := len(config.BootstrapPeers) + len(config.Gateways); configured > 0 {
			return nil, fmt.Errorf(
				"none of the %d configured bootstrap peers and gateways is a valid address, there is nowhere to fetch from",
				configured,
			)
		}

		return nil, errors.New("no bootstrap peers, gateways or delegated routing endpoint configured, there is nowhere to fetch from")
	}

	return &Client{
		config:              node,
		idle:                config.IdleTimeout,
		gateways:            config.Gateways,
		allowUnverified:     config.AllowUnverifiedGatewayFallback,
		primaryTimeout:      orDefault(config.PrimaryTimeout, defaultPrimaryTimeout),
		fallbackStepTimeout: orDefault(config.FallbackStepTimeout, defaultFallbackStepTimeout),
	}, nil
}

func orDefault(configured time.Duration, fallback time.Duration) time.Duration {
	if configured <= 0 {
		return fallback
	}

	return configured
}

// Get downloads cid to output, giving up when ctx is done. A sizeLimit above
// zero rejects larger content before it is written.
//
// The primary phase fetches from peers and gateways together, verifying every
// block. Only once that fails, and Config.AllowUnverifiedGatewayFallback is set,
// does it fall back to an unverified gateway fetch.
//
// On success output is replaced, whether or not something was already there.
func (client *Client) Get(ctx context.Context, cidStr string, output string, sizeLimit int64) error {
	// Parsed before either path runs, and passed down as a value from here on.
	// Both paths derive a filesystem path or a URL from it, and neither is safe
	// to build from an unvalidated caller string.
	parsed, err := cid.Parse(cidStr)
	if err != nil {
		return fmt.Errorf("invalid cid %q: %w", cidStr, err)
	}

	primary, cancel := client.primaryDeadline(ctx)
	defer cancel()

	err = client.getPrimary(primary, parsed, output, sizeLimit)
	if err == nil {
		return nil
	}

	// Closed means closed: no fallback, no network.
	if errors.Is(err, ErrClosed) {
		return err
	}

	// No other source returns smaller content.
	var tooBig *SizeLimitError
	if errors.As(err, &tooBig) {
		return err
	}

	// Only the primary phase's own deadline leaves budget for anything else; the
	// caller's deadline or cancellation ends it here.
	if ctx.Err() != nil {
		return contextError(ctx)
	}

	if !client.fallbackAvailable() {
		return err
	}

	// Close may have landed while the primary phase was running. The fallback
	// holds no node, so nothing else would stop a retired client from reaching
	// the network and writing output.
	if client.isClosed() {
		return ErrClosed
	}

	fmt.Printf("primary retrieval of %s failed (%s), trying gateways unverified\n", parsed, err)

	fallbackErr := client.fetchFromGateways(ctx, parsed, output, sizeLimit)
	if fallbackErr == nil {
		return nil
	}

	// The Kotlin wrapper selects SizeLimitExceededException on the message prefix,
	// so this must not be buried behind the primary failure by errors.Join.
	if errors.As(fallbackErr, &tooBig) {
		return fallbackErr
	}

	return errors.Join(err, fallbackErr)
}

// primaryShare caps how much of the caller's remaining time the primary phase
// may spend, so a hanging route still leaves budget to fall back with.
const primaryShare = 0.75

func (client *Client) isClosed() bool {
	client.mutex.Lock()
	defer client.mutex.Unlock()

	return client.closed
}

func (client *Client) fallbackAvailable() bool {
	return client.allowUnverified && len(client.gateways) > 0
}

func (client *Client) primaryDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if !client.fallbackAvailable() {
		return context.WithCancel(ctx)
	}

	// Always bounded: without this the primary phase runs until the caller's
	// deadline, or forever when there is none, and the fallback never starts.
	budget := client.primaryTimeout

	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			budget = min(budget, time.Duration(float64(remaining)*primaryShare))
		}
	}

	return context.WithTimeout(ctx, budget)
}

// getPrimary fetches over the block exchange, where every block is checked
// against the CID that asked for it.
func (client *Client) getPrimary(ctx context.Context, target cid.Cid, output string, sizeLimit int64) error {
	result := make(chan error, 1)

	// Started on its own goroutine so the select below returns at the deadline
	// even if something further down stops honouring ctx. Node startup runs under
	// ctx too: dialling peers is usually the slowest part of a cold fetch.
	go func() {
		node, err := client.acquire(ctx)
		if err != nil {
			result <- err
			return
		}
		defer client.release()

		result <- node.download(ctx, target, output, sizeLimit)
	}()

	select {
	case err := <-result:
		// When the deadline expires both cases are ready and select would pick at
		// random, so decide on ctx rather than on which channel won.
		if err != nil && ctx.Err() != nil {
			return contextError(ctx)
		}

		return err
	case <-ctx.Done():
		return contextError(ctx)
	}
}

// contextError maps a finished context onto the error the caller sees. The
// Kotlin wrapper surfaces these messages, so a deadline reports as "timeout".
func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return utils.Timeout()
	}

	return ctx.Err()
}

// Close shuts the node down and retires the Client. Safe to call more than once.
// Downloads still running are cancelled.
func (client *Client) Close() error {
	client.mutex.Lock()

	if client.closed {
		client.mutex.Unlock()
		return nil
	}

	client.closed = true
	client.stopTimer()

	node := client.node
	client.node = nil

	client.mutex.Unlock()

	// Teardown waits on libp2p and bitswap shutting down, so it must not hold the
	// mutex: a concurrent Get would stall behind it with no way out.
	if node != nil {
		node.close()
	}

	return nil
}

// acquire returns a running node, starting one if necessary, and records that a
// download is using it so the idle timer cannot close it mid-flight.
func (client *Client) acquire(ctx context.Context) (*node, error) {
	client.mutex.Lock()

	if client.closed {
		client.mutex.Unlock()
		return nil, ErrClosed
	}

	client.stopTimer()

	if client.node != nil {
		node := client.node
		client.inflight++
		client.mutex.Unlock()

		return node, nil
	}

	// Claimed before unlocking, so a concurrent Close or idle sweep sees the
	// download and leaves the node alone.
	client.inflight++
	client.mutex.Unlock()

	node, err := startNode(ctx, client.config)
	if err != nil {
		client.release()
		return nil, err
	}

	client.mutex.Lock()

	switch {
	case client.closed:
		client.mutex.Unlock()
		node.close()
		client.release()

		return nil, ErrClosed
	case client.node != nil:
		// Another download started one first. Keep theirs.
		existing := client.node
		client.mutex.Unlock()
		node.close()

		return existing, nil
	default:
		client.node = node
		client.mutex.Unlock()

		return node, nil
	}
}

// release records that a download finished and arms the idle timer once the node
// is no longer in use.
func (client *Client) release() {
	client.mutex.Lock()
	defer client.mutex.Unlock()

	client.inflight--

	if client.inflight > 0 || client.closed || client.idle <= 0 {
		return
	}

	client.stopTimer()
	client.timer = time.AfterFunc(client.idle, client.closeIdle)
}

func (client *Client) closeIdle() {
	client.mutex.Lock()

	if client.closed || client.inflight > 0 || client.node == nil {
		client.mutex.Unlock()
		return
	}

	node := client.node
	client.node = nil
	client.timer = nil

	client.mutex.Unlock()

	node.close()
}

// stopTimer must be called with the mutex held.
func (client *Client) stopTimer() {
	if client.timer != nil {
		client.timer.Stop()
		client.timer = nil
	}
}

// Get downloads a single CID through a Client created and closed around the call.
// It re-dials the bootstrap peers every time, so prefer reusing one Client.
func Get(ctx context.Context, cid string, output string, config *Config, sizeLimit int64) error {
	client, err := New(config)
	if err != nil {
		return err
	}
	defer client.Close()

	return client.Get(ctx, cid, output, sizeLimit)
}
