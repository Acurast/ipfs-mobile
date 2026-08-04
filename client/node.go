package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"

	"github.com/multiformats/go-multiaddr"

	bsclient "github.com/ipfs/boxo/bitswap/client"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/ipld/merkledag"
	unixfile "github.com/ipfs/boxo/ipld/unixfs/file"
)

const (
	// Upper bound on waiting for the DHT routing table to become usable. A cold
	// start against real bootstrap peers normally fills it well inside this.
	dhtReadyTimeout = 5 * time.Second
	dhtReadyPoll    = 50 * time.Millisecond
)

// node is a running libp2p host with a bitswap client attached, and a DHT to
// find providers with. It is owned by exactly one Client, which is the only
// thing allowed to close it.
type node struct {
	cancel context.CancelFunc
	host   host.Host
	dht    *dht.IpfsDHT
	bs     *bsclient.Client
}

// nodeConfig is everything a node needs to start. It is built once, when the
// Client is created, and reused every time the node is restarted.
type nodeConfig struct {
	port       int32
	peers      []peer.AddrInfo
	disableDHT bool

	// delegated is nil unless a delegated routing endpoint was configured.
	delegated routing.ContentDiscovery
}

// startNode brings up a host and connects it to the bootstrap peers. It blocks
// until the first peer is reachable, bounded by ctx: bitswap has nothing to ask
// until it has a connection, so failing here beats letting the download hang.
func startNode(ctx context.Context, config nodeConfig) (*node, error) {
	host, err := makeHost(config.port)
	if err != nil {
		return nil, err
	}

	// The node outlives any single request, so its lifetime is not tied to ctx.
	nodeCtx, cancel := context.WithCancel(context.Background())

	node := &node{cancel: cancel, host: host}

	// Without any provider finder bitswap has no content routing at all, and can
	// only fetch from peers it happens to be directly connected to.
	var kadFinder routing.ContentDiscovery

	if !config.disableDHT {
		// Client mode: query the DHT without answering queries for it. A phone is
		// usually behind NAT and on a metered, battery powered connection, so it
		// makes a poor DHT server.
		//
		// No context here: the DHT's lifetime is bounded by its own Close, which
		// node.close calls.
		kad, err := dht.New(host, dht.Mode(dht.ModeClient), dht.BootstrapPeers(config.peers...))
		if err != nil {
			node.close()
			return nil, fmt.Errorf("starting the dht: %w", err)
		}

		node.dht = kad
		kadFinder = kad
	}

	network := bsnet.NewFromIpfsHost(host)
	node.bs = bsclient.New(
		nodeCtx,
		network,
		newProviderFinder(kadFinder, config.delegated),
		blockstore.NewBlockstore(datastore.NewNullDatastore()),
	)
	network.Start(node.bs)

	if err := connectToPeers(ctx, host, config.peers); err != nil {
		node.close()
		return nil, err
	}

	if node.dht != nil {
		node.bootstrapDHT(ctx)
	}

	return node, nil
}

// bootstrapDHT populates the routing table and waits, briefly, for it to hold at
// least one peer. A provider lookup against an empty table finds nothing, so
// starting the download before then spends the caller's deadline on a query that
// cannot yet succeed.
//
// Failing to get a usable table is never fatal: directly connected peers may
// still hold the content, and the table keeps filling while the download runs.
func (node *node) bootstrapDHT(ctx context.Context) {
	if err := node.dht.Bootstrap(ctx); err != nil {
		fmt.Printf("failed to bootstrap the dht: %s\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, dhtReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(dhtReadyPoll)
	defer ticker.Stop()

	for node.dht.RoutingTable().Size() == 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			fmt.Printf("dht routing table still empty after %s, continuing anyway\n", dhtReadyTimeout)
			return
		}
	}
}

func (node *node) download(ctx context.Context, cidStr string, output string, sizeLimit int64) error {
	// cid.MustParse panics on a malformed CID, which would take the process down
	// rather than surfacing as an error to the caller.
	parsed, err := cid.Parse(cidStr)
	if err != nil {
		return fmt.Errorf("invalid cid %q: %w", cidStr, err)
	}

	bserv := blockservice.New(blockstore.NewBlockstore(datastore.NewNullDatastore()), node.bs)
	session := merkledag.NewSession(ctx, merkledag.NewDAGService(bserv))
	dserv := merkledag.NewReadOnlyDagService(session)

	nd, err := dserv.Get(ctx, parsed)
	if err != nil {
		return err
	}

	unixfsnd, err := unixfile.NewUnixfsFile(ctx, dserv, nd)
	if err != nil {
		return err
	}

	if sizeLimit > 0 {
		size, err := unixfsnd.Size()
		if err != nil {
			return err
		}

		if size > sizeLimit {
			return fmt.Errorf("size limit exceeded (actual size = %d limit = %d bytes)", size, sizeLimit)
		}
	}

	// A download that hits its deadline is abandoned part way through the write,
	// so stage it next to the target and move it into place only once complete.
	// Otherwise a timed out call leaves a truncated file at output, and a retry
	// races the abandoned write for the same path.
	scratch, err := os.MkdirTemp(filepath.Dir(output), ".ipfs-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	staged := filepath.Join(scratch, "data")
	if err := files.WriteTo(unixfsnd, staged); err != nil {
		return err
	}

	if err := os.RemoveAll(output); err != nil {
		return err
	}

	return os.Rename(staged, output)
}

// close tolerates a partially built node, since startNode calls it to unwind a
// failed start.
func (node *node) close() {
	node.cancel()

	if node.bs != nil {
		node.bs.Close()
	}
	if node.dht != nil {
		node.dht.Close()
	}

	node.host.Close()
}

func makeHost(port int32) (host.Host, error) {
	// Ed25519 rather than RSA-2048: the identity is ephemeral, and RSA key
	// generation costs seconds on mobile ARM cores with a long tail.
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, err
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)),
		libp2p.Identity(priv),
	}

	return libp2p.New(opts...)
}

// parsePeers validates bootstrap addresses up front so a bad list fails when the
// Client is built, rather than producing a node with nobody to talk to. Invalid
// entries are skipped; only an entirely unusable list is an error.
func parsePeers(addrs []string) ([]peer.AddrInfo, error) {
	infos := make(map[peer.ID]*peer.AddrInfo, len(addrs))
	order := make([]peer.ID, 0, len(addrs))

	for _, addrStr := range addrs {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			fmt.Printf("skipping invalid bootstrap peer %q: %s\n", addrStr, err)
			continue
		}

		parsed, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			fmt.Printf("skipping invalid bootstrap peer %q: %s\n", addrStr, err)
			continue
		}

		info, seen := infos[parsed.ID]
		if !seen {
			info = &peer.AddrInfo{ID: parsed.ID}
			infos[info.ID] = info
			order = append(order, info.ID)
		}

		info.Addrs = append(info.Addrs, parsed.Addrs...)
	}

	if len(infos) == 0 {
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no bootstrap peers configured, there is nobody to fetch blocks from")
		}

		return nil, fmt.Errorf("none of the %d configured bootstrap peers is a valid address", len(addrs))
	}

	// Keep the configured order rather than the map's, so behaviour is stable.
	peers := make([]peer.AddrInfo, 0, len(order))
	for _, id := range order {
		peers = append(peers, *infos[id])
	}

	return peers, nil
}

// connectToPeers dials every peer in parallel and returns as soon as one of them
// answers. The rest keep dialling in the background: more peers is strictly
// better for bitswap, but waiting for the slowest one is not.
func connectToPeers(ctx context.Context, host host.Host, peers []peer.AddrInfo) error {
	if len(peers) == 0 {
		return fmt.Errorf("no bootstrap peers configured, there is nobody to fetch blocks from")
	}

	var (
		wg        sync.WaitGroup
		once      sync.Once
		connected atomic.Int32
	)

	first := make(chan struct{})
	wg.Add(len(peers))

	for _, peerInfo := range peers {
		go func(peerInfo peer.AddrInfo) {
			defer wg.Done()

			if err := host.Connect(ctx, peerInfo); err != nil {
				fmt.Printf("failed to connect to %s: %s\n", peerInfo.ID, err)
				return
			}

			connected.Add(1)
			once.Do(func() { close(first) })
		}(peerInfo)
	}

	exhausted := make(chan struct{})
	go func() {
		wg.Wait()
		close(exhausted)
	}()

	select {
	case <-first:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-exhausted:
		// Every dial finished. A success racing the last failure would leave both
		// channels ready, so re-check rather than trusting the select.
		if connected.Load() > 0 {
			return nil
		}

		return fmt.Errorf("failed to connect to any of the %d bootstrap peers", len(peers))
	}
}
