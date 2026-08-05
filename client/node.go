package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/ipfs/boxo/bitswap/network"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/bitswap/network/httpnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/ipld/merkledag"
	unixfile "github.com/ipfs/boxo/ipld/unixfs/file"
)

const (
	// Upper bound on waiting for a usable DHT routing table.
	dhtReadyTimeout = 5 * time.Second
	dhtReadyPoll    = 50 * time.Millisecond
)

// node is a running libp2p host with a block exchange and a DHT, owned by
// exactly one Client.
type node struct {
	cancel   context.CancelFunc
	host     host.Host
	dht      *dht.IpfsDHT
	http     network.BitSwapNetwork
	exchange network.BitSwapNetwork
	bs       *bsclient.Client
}

// nodeConfig is everything a node needs to start.
type nodeConfig struct {
	port       int32
	peers      []peer.AddrInfo
	disableDHT bool

	// gateways are asked at the same time as libp2p peers, not after them.
	gateways []peer.AddrInfo

	// gatewayHosts are the hosts of gateways, which is the allowlist the HTTP
	// exchange is held to.
	gatewayHosts []string

	delegated routing.ContentDiscovery
}

// startNode brings up a host and blocks until it has somewhere to fetch from,
// bounded by ctx. Failing here beats letting the download hang on a node with no
// connections.
func startNode(ctx context.Context, config nodeConfig) (*node, error) {
	host, err := makeHost(config.port)
	if err != nil {
		return nil, err
	}

	// The node outlives any single request, so its lifetime is not tied to ctx.
	nodeCtx, cancel := context.WithCancel(context.Background())

	node := &node{cancel: cancel, host: host}

	var kadFinder routing.ContentDiscovery

	if !config.disableDHT {
		// Client mode: a phone behind NAT on a metered connection makes a poor DHT
		// server, so query without answering.
		kad, err := dht.New(host, dht.Mode(dht.ModeClient), dht.BootstrapPeers(config.peers...))
		if err != nil {
			node.close()
			return nil, fmt.Errorf("starting the dht: %w", err)
		}

		node.dht = kad
		kadFinder = kad
	}

	// One exchange over two transports, so a gateway and a libp2p peer are both
	// just peers that might have the block.
	// Only the configured gateways may be reached over HTTP. Content routing hands
	// back addresses chosen by whoever answered the lookup, and an HTTP address
	// becomes a GET - so without this a hostile provider record turns the device
	// into a probe against its own network. Blocks are hashed either way, so the
	// exposure is egress rather than content, but the library already knows which
	// HTTP hosts are legitimate and there is no reason to talk to others.
	//
	// With no gateways configured there is nothing to allow, so the HTTP half is
	// left out entirely and the router runs bitswap alone.
	if len(config.gatewayHosts) > 0 {
		node.http = httpnet.New(
			host,
			httpnet.WithUserAgent(userAgent),
			httpnet.WithAllowlist(config.gatewayHosts),
		)
	}

	node.exchange = network.New(host.Peerstore(), bsnet.NewFromIpfsHost(host), node.http)

	node.bs = bsclient.New(
		nodeCtx,
		node.exchange,
		newProviderFinder(kadFinder, config.delegated),
		blockstore.NewBlockstore(datastore.NewNullDatastore()),
	)
	node.exchange.Start(node.bs)

	// Dialled on the node's own context, not the caller's: these outlive the
	// download that happened to start the node, and cancelling them with it would
	// leave a reused node stuck on whichever peer answered first.
	gateways := connectToGateways(ctx, nodeCtx, node.http, config.gateways)

	// Peers, gateways and an indexer are each enough on their own.
	reachable := gateways > 0 || config.delegated != nil

	if err := connectToPeers(ctx, nodeCtx, host, config.peers); err != nil {
		if !reachable {
			node.close()
			return nil, err
		}

		fmt.Printf("continuing without libp2p peers: %s\n", err)
	}

	if node.dht != nil {
		node.bootstrapDHT(ctx)
	}

	return node, nil
}

// bootstrapDHT waits briefly for the routing table to hold a peer, since a
// lookup against an empty one finds nothing and spends the caller's deadline
// doing it. Never fatal: connected peers may hold the content anyway, and the
// table keeps filling during the download.
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

func (node *node) download(ctx context.Context, target cid.Cid, output string, sizeLimit int64) error {
	bserv := blockservice.New(blockstore.NewBlockstore(datastore.NewNullDatastore()), node.bs)
	session := merkledag.NewSession(ctx, merkledag.NewDAGService(bserv))
	dserv := merkledag.NewReadOnlyDagService(session)

	nd, err := dserv.Get(ctx, target)
	if err != nil {
		return err
	}

	unixfsnd, err := unixfile.NewUnixfsFile(ctx, dserv, nd)
	if err != nil {
		return err
	}

	// A hint only, and cheap: the DAG author writes this figure and nothing
	// reconciles it with the leaves it links. Enforcement is on bytes written.
	if sizeLimit > 0 {
		size, err := unixfsnd.Size()
		if err != nil {
			return err
		}

		if size > sizeLimit {
			return &SizeLimitError{Size: size, Limit: sizeLimit}
		}
	}

	// Staged and renamed, so a download abandoned at its deadline leaves no
	// truncated file at output and does not race a retry writing the same path.
	// output is replaced if it already exists.
	sweepScratch(filepath.Dir(output))

	scratch, err := os.MkdirTemp(filepath.Dir(output), scratchPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	staged := filepath.Join(scratch, "data")
	if _, err := materialise(unixfsnd, staged, sizeLimit, 0); err != nil {
		return err
	}

	if err := os.RemoveAll(output); err != nil {
		return err
	}

	return os.Rename(staged, output)
}

const scratchPrefix = ".ipfs-download-"

// How old a staging directory must be before it is treated as the remains of a
// process that died rather than a download still running.
const scratchStaleAfter = time.Hour

// sweepScratch removes staging directories left behind by a previous run. The
// deferred cleanup only covers a live process, and on mobile the app is killed
// mid-download often enough that partial copies would otherwise accumulate in
// the data directory without bound.
func sweepScratch(dir string) {
	sweepScratchPrefix(dir, scratchPrefix)
}

func sweepScratchPrefix(dir string, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Age matters: concurrent downloads stage alongside each other, so
		// removing every match would delete a sibling's work in progress.
		if time.Since(info.ModTime()) < scratchStaleAfter {
			continue
		}

		os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// materialise writes nd at path and returns how many content bytes it wrote.
//
// Deliberately not files.WriteTo, which reproduces the DAG faithfully - and a
// DAG is richer than a byte string. It can hold a symlink, whose target the DAG
// author chooses and which os.Symlink would write verbatim, so a caller that
// reads the result reads a file of the author's choosing instead. It can also
// declare any size it likes, so the limit has to hold against arriving bytes.
//
// Only regular files and directories are written; anything else is refused.
func materialise(nd files.Node, path string, sizeLimit int64, written int64) (int64, error) {
	switch node := nd.(type) {
	// Ahead of files.File, which a Symlink also satisfies.
	case *files.Symlink:
		return written, fmt.Errorf("refusing to write a symlink at %q", filepath.Base(path))

	case files.File:
		remaining := int64(-1)
		if sizeLimit > 0 {
			remaining = sizeLimit - written
		}

		count, err := writeLimited(path, node, remaining)

		return written + count, err

	case files.Directory:
		if err := os.Mkdir(path, 0o755); err != nil {
			return written, err
		}

		entries := node.Entries()
		for entries.Next() {
			name := entries.Name()
			if !validEntryName(name) {
				return written, fmt.Errorf("invalid directory entry name %q", name)
			}

			var err error
			written, err = materialise(entries.Node(), filepath.Join(path, name), sizeLimit, written)
			if err != nil {
				return written, err
			}
		}

		return written, entries.Err()

	default:
		return written, fmt.Errorf("unsupported unixfs node type %T", nd)
	}
}

// validEntryName refuses names that would place an entry outside the directory
// being written.
func validEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

// close tolerates a partially built node, which is how startNode unwinds.
func (node *node) close() {
	node.cancel()

	if node.bs != nil {
		node.bs.Close()
	}

	// The whole exchange, not just the HTTP half: stopping only one leaves the
	// other's connection event worker and host notifiee running for the life of
	// the process.
	if node.exchange != nil {
		node.exchange.Stop()
	}

	if node.dht != nil {
		node.dht.Close()
	}

	node.host.Close()
}

func makeHost(port int32) (host.Host, error) {
	// Ed25519 because the identity is ephemeral and RSA keygen costs seconds on
	// mobile ARM cores.
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

// parsePeers validates bootstrap addresses when the Client is built rather than
// at the first download. Invalid entries are skipped, and an empty result is not
// an error: gateways or an indexer may be the only configured route.
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

	if len(infos) == 0 && len(addrs) > 0 {
		return nil, fmt.Errorf("none of the %d configured bootstrap peers is a valid address", len(addrs))
	}

	// Configured order rather than the map's, so behaviour is stable.
	peers := make([]peer.AddrInfo, 0, len(order))
	for _, id := range order {
		peers = append(peers, *infos[id])
	}

	return peers, nil
}

// connectToGateways registers each gateway and reports how many answered.
// Connecting means the endpoint responded to a probe, not that a libp2p
// handshake completed. Unreachable gateways are logged, never fatal.
func connectToGateways(ctx context.Context, nodeCtx context.Context, exchange network.BitSwapNetwork, gateways []peer.AddrInfo) int {
	if len(gateways) == 0 {
		return 0
	}

	var (
		wait      sync.WaitGroup
		connected atomic.Int32
	)

	wait.Add(len(gateways))

	for _, gateway := range gateways {
		go func(gateway peer.AddrInfo) {
			defer wait.Done()

			if err := exchange.Connect(nodeCtx, gateway); err != nil {
				fmt.Printf("gateway %s unreachable: %s\n", gatewayName(gateway), err)
				return
			}

			connected.Add(1)
		}(gateway)
	}

	wait.Wait()

	return int(connected.Load())
}

// gatewayName renders a gateway for logs, where its synthetic peer ID would be
// noise.
func gatewayName(gateway peer.AddrInfo) string {
	if len(gateway.Addrs) == 0 {
		return gateway.ID.String()
	}

	return gateway.Addrs[0].String()
}

// connectToPeers dials every peer in parallel and returns once one answers,
// bounded by ctx. The rest keep dialling on nodeCtx rather than holding up the
// caller, and so survive the download that started them.
func connectToPeers(ctx context.Context, nodeCtx context.Context, host host.Host, peers []peer.AddrInfo) error {
	if len(peers) == 0 {
		return fmt.Errorf("no bootstrap peers configured")
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

			if err := host.Connect(nodeCtx, peerInfo); err != nil {
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
