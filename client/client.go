package client

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"ipfs-mobile/utils"
)

// ErrClosed is returned by Get once the Client has been closed.
var ErrClosed = errors.New("client is closed")

type Config struct {
	BootstrapPeers []string
	Port           int32

	// IdleTimeout closes the underlying node once it has gone this long without
	// a download, so an idle client stops holding connections open. Zero or
	// negative disables it and leaves the node running until Close is called.
	//
	// Either way the node is restarted on demand, so a Client stays usable for
	// as long as it is open.
	IdleTimeout time.Duration
}

// Client is a reusable handle over an IPFS node. The node is started on the
// first download and reused by later ones, so bootstrap peers are dialled once
// rather than per fetch.
//
// A Client is safe for concurrent use. Close must be called to release it,
// unless Config.IdleTimeout is set, in which case an unused node shuts itself
// down and Close only needs to be called to retire the Client for good.
type Client struct {
	port  int32
	peers []peer.AddrInfo
	idle  time.Duration

	mutex    sync.Mutex
	node     *node
	inflight int
	timer    *time.Timer
	closed   bool
}

// New validates the configuration and returns a Client. The node itself is not
// started until the first download.
func New(config *Config) (*Client, error) {
	peers, err := parsePeers(config.BootstrapPeers)
	if err != nil {
		return nil, err
	}

	return &Client{port: config.Port, peers: peers, idle: config.IdleTimeout}, nil
}

// Get downloads cid to output, giving up when ctx is done. A sizeLimit above
// zero rejects content larger than that many bytes before it is written.
func (client *Client) Get(ctx context.Context, cid string, output string, sizeLimit int64) error {
	result := make(chan error, 1)

	// Node startup runs under ctx as well: it dials bootstrap peers, which is
	// often the slowest part of a cold fetch, and the caller's deadline has to
	// cover it. The select below then guarantees a return at the deadline even
	// if something further down stops honouring ctx.
	go func() {
		node, err := client.acquire(ctx)
		if err != nil {
			result <- err
			return
		}
		defer client.release()

		result <- node.download(ctx, cid, output, sizeLimit)
	}()

	select {
	case err := <-result:
		// A failure that lands after the deadline has already passed is a
		// timeout, whatever it reports internally. Checking ctx here also keeps
		// the outcome deterministic: when the deadline expires both this case
		// and the one below are ready, and select would otherwise pick at random.
		if err != nil && ctx.Err() != nil {
			return contextError(ctx)
		}

		return err
	case <-ctx.Done():
		return contextError(ctx)
	}
}

// contextError maps a finished context onto the error the caller sees. The
// Kotlin wrapper surfaces these messages directly, so a deadline has to keep
// reporting as "timeout".
func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return utils.Timeout()
	}

	return ctx.Err()
}

// Close shuts the node down and retires the Client. It is safe to call more
// than once. Downloads still running are cancelled.
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

	// Teardown blocks: host.Close waits on the host's background goroutines and
	// bitswap's Close waits for its own shutdown. Neither may run under the
	// mutex, or a concurrent Get would stall behind them with no way out.
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

	// Reserve the slot before unlocking so a concurrent Close or idle sweep sees
	// the download and leaves the node alone.
	client.inflight++
	client.mutex.Unlock()

	node, err := startNode(ctx, client.port, client.peers)
	if err != nil {
		client.release()
		return nil, err
	}

	client.mutex.Lock()

	switch {
	case client.closed:
		// Closed while we were starting up: this node belongs to nobody.
		client.mutex.Unlock()
		node.close()
		client.release()

		return nil, ErrClosed
	case client.node != nil:
		// Another download won the race and started one first. Keep theirs.
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

// release records that a download finished and arms the idle timer once the
// node is no longer in use.
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

// closeIdle shuts the node down after Config.IdleTimeout with no downloads. The
// next Get simply starts a new one.
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

// Get downloads a single CID through a Client that is created and closed around
// the call. It re-dials the bootstrap peers every time, so prefer building one
// Client and reusing it when fetching more than once.
func Get(ctx context.Context, cid string, output string, config *Config, sizeLimit int64) error {
	client, err := New(config)
	if err != nil {
		return err
	}
	defer client.Close()

	return client.Get(ctx, cid, output, sizeLimit)
}
