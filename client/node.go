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
	libp2pnet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	websocket "github.com/libp2p/go-libp2p/p2p/transport/websocket"

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
	// How long startup waits for the first peer or gateway to answer. Dialling
	// carries on past it; only the waiting stops.
	startupTimeout = 2 * time.Second

	// Above the high water mark connections are trimmed back to the low one, new
	// ones are exempt for the grace period, and the manager looks for work at the
	// trim interval. Well under go-libp2p's own, which are a server's: a routing
	// lookup fills whatever it is given, and on a phone every connection costs
	// battery and a slot in the carrier's NAT table.
	//
	// The grace period is short enough to trim during the burst a lookup opens
	// rather than after it has passed.
	lowWaterConnections    = 8
	highWaterConnections   = 16
	connectionGracePeriod  = 2 * time.Second
	connectionTrimInterval = 3 * time.Second

	// Peers named in the configuration are the ones holding the content, so a
	// lookup filling the connection table must not displace them.
	configuredPeerTag = "configured"

	// How many peers a lookup queries at once. Narrower than the default, which
	// trades a few more rounds per lookup for a burst a phone survives.
	dhtQueryConcurrency = 3
)

// node is a running libp2p host with a block exchange and a DHT, owned by
// exactly one Client.
type node struct {
	cancel context.CancelFunc
	// lifetime outlives any single download and is cancelled by close, so dials
	// started later are bounded by the node rather than by whoever asked.
	lifetime context.Context
	host     host.Host
	dht      *dht.IpfsDHT
	http     network.BitSwapNetwork
	exchange network.BitSwapNetwork
	bs       *bsclient.Client

	// The dials last started, so a retry does not stack on one still running.
	peerDials    *dialGroup
	gatewayDials *dialGroup
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

	// resolver is what libp2p resolves addresses with. Nil leaves it its own.
	resolver libp2pnet.MultiaddrDNSResolver

	// onNodeStarted fires once a host is built, which tests count.
	onNodeStarted func()
}

// startNode brings up a host and gives its dials a short head start, bounded by
// ctx. It fails only when there is provably nowhere to fetch from.
func startNode(ctx context.Context, config nodeConfig) (*node, error) {
	host, err := makeHost(config)
	if err != nil {
		return nil, err
	}

	// The node outlives any single request, so its lifetime is not tied to ctx.
	nodeCtx, cancel := context.WithCancel(context.Background())

	node := &node{cancel: cancel, lifetime: nodeCtx, host: host}

	var kadFinder routing.ContentDiscovery

	if !config.disableDHT {
		// Client mode: a phone behind NAT on a metered connection makes a poor DHT
		// server, so query without answering.
		kad, err := dht.New(
			host,
			dht.Mode(dht.ModeClient),
			dht.BootstrapPeers(config.peers...),
			dht.Concurrency(dhtQueryConcurrency),
		)
		if err != nil {
			node.close()
			return nil, fmt.Errorf("starting the dht: %w", err)
		}

		node.dht = kad
		kadFinder = kad
	}

	// One exchange over two transports, so a gateway and a libp2p peer are both
	// just peers that might have the block.
	//
	// Held to the configured gateways: content routing hands back addresses chosen
	// by whoever answered the lookup, and an HTTP address becomes a GET, so a
	// hostile provider record would otherwise aim the device at its own network.
	// Blocks are hashed either way, so the exposure is egress rather than content.
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

	// A lookup meets far more peers than the configuration names. The DHT protects
	// the ones it puts in its routing table; a peer that only serves content never
	// gets there, so it is protected here instead.
	for _, peerInfo := range config.peers {
		host.ConnManager().Protect(peerInfo.ID, configuredPeerTag)
	}

	// Dialled on the node's own context, not the caller's: these outlive the
	// download that happened to start the node, and cancelling them with it would
	// leave a reused node stuck on whichever peer answered first.
	gateways := dialAll(nodeCtx, config.gateways, func(ctx context.Context, gateway peer.AddrInfo) error {
		return node.http.Connect(ctx, gateway)
	}, gatewayName)

	peers := dialAll(nodeCtx, config.peers, host.Connect, peerName)

	node.gatewayDials, node.peerDials = gateways, peers

	// Bitswap asks whoever is connected and asks again as others arrive, so
	// startup only needs somewhere to send the first request.
	awaitConnection(ctx, gateways, peers)

	// Peers, gateways and an indexer are each enough on their own.
	if gateways.count() == 0 && peers.count() == 0 && config.delegated == nil {
		// A dial still in flight can land mid-download, so only a node with
		// nothing left to try has provably nowhere to go.
		if gateways.finished() && peers.finished() {
			node.close()

			if ctx.Err() != nil {
				return nil, contextError(ctx)
			}

			return nil, fmt.Errorf(
				"none of the %d bootstrap peers and %d gateways could be reached",
				len(config.peers), len(config.gateways),
			)
		}

		fmt.Println("no peer or gateway answered yet, starting the download anyway")
	}

	if node.dht != nil {
		// On the node's context: the table fills from the same dials, and a
		// download that has peers to ask does not need to wait for it.
		go node.bootstrapDHT(nodeCtx)
	}

	if config.onNodeStarted != nil {
		config.onNodeStarted()
	}

	return node, nil
}

// redial retries the configured peers and gateways when nothing is connected.
//
// A node is kept for as long as the client stays warm, and startNode hands it
// back as soon as one dial is still in flight - so dials that then all failed,
// or connections lost to a network change, would otherwise leave it cached with
// nowhere to fetch from until it goes idle.
func (node *node) redial(config nodeConfig) {
	if len(node.host.Network().Peers()) > 0 {
		return
	}
	if node.peerDials != nil && !node.peerDials.finished() {
		return
	}

	node.peerDials = dialAll(node.lifetime, config.peers, node.host.Connect, peerName)

	if node.http != nil {
		node.gatewayDials = dialAll(node.lifetime, config.gateways, func(ctx context.Context, gateway peer.AddrInfo) error {
			return node.http.Connect(ctx, gateway)
		}, gatewayName)
	}
}

// awaitConnection waits for one peer or gateway to answer, giving up once every
// dial has settled, startupTimeout passes, or the caller's deadline arrives -
// whichever comes first.
func awaitConnection(ctx context.Context, gateways, peers *dialGroup) {
	wait, stop := context.WithTimeout(ctx, startupTimeout)
	defer stop()

	exhausted := make(chan struct{})
	go func() {
		<-gateways.done
		<-peers.done
		close(exhausted)
	}()

	select {
	case <-gateways.first:
	case <-peers.first:
	case <-exhausted:
	case <-wait.Done():
	}
}

// bootstrapDHT starts the routing table filling. Never fatal: connected peers
// may hold the content anyway, and the table keeps filling during the download.
func (node *node) bootstrapDHT(ctx context.Context) {
	if err := node.dht.Bootstrap(ctx); err != nil {
		fmt.Printf("failed to bootstrap the dht: %s\n", err)
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

	scratch, release, err := claimScratch(filepath.Dir(output), scratchPrefix)
	if err != nil {
		return err
	}
	defer release()

	staged := filepath.Join(scratch, "data")
	if _, err := materialise(unixfsnd, staged, sizeLimit, 0); err != nil {
		return err
	}

	return replace(ctx, staged, output)
}

const scratchPrefix = ".ipfs-download-"

// How old a staging directory must be before it is treated as the remains of a
// process that died rather than a download still running.
const scratchStaleAfter = time.Hour

// Staging directories this process is writing into, which the sweep leaves alone
// however old they look: writing a file does not advance the modified time of the
// directory holding it, so age cannot tell a slow download from an abandoned one.
var staging sync.Map

// claimScratch creates a staging directory under dir, returning it with the
// function that releases and removes it.
func claimScratch(dir string, prefix string) (string, func(), error) {
	scratch, err := os.MkdirTemp(dir, prefix)
	if err != nil {
		return "", nil, err
	}

	staging.Store(scratch, struct{}{})

	return scratch, func() {
		staging.Delete(scratch)
		os.RemoveAll(scratch)
	}, nil
}

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

		path := filepath.Join(dir, entry.Name())

		// A download running right now, however long it has been going.
		if _, live := staging.Load(path); live {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Everything else belongs to some other process, which may still be alive
		// and writing. Age is the only signal available about one of those.
		if time.Since(info.ModTime()) < scratchStaleAfter {
			continue
		}

		os.RemoveAll(path)
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

		return written + count, sizeLimitWithTotals(err, written+count, sizeLimit)

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

func makeHost(config nodeConfig) (host.Host, error) {
	// Ed25519 because the identity is ephemeral and RSA keygen costs seconds on
	// mobile ARM cores.
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, err
	}

	connections, err := connmgr.NewConnManager(
		lowWaterConnections,
		highWaterConnections,
		connmgr.WithGracePeriod(connectionGracePeriod),
		connmgr.WithSilencePeriod(connectionTrimInterval),
	)
	if err != nil {
		return nil, fmt.Errorf("building the connection manager: %w", err)
	}

	opts := []libp2p.Option{
		listenOn(config.port),
		libp2p.Identity(priv),
		libp2p.ConnectionManager(connections),
		libp2p.ConnectionGater(newDiscoveredAddrGater(config.peers)),

		// Noise before TLS, reversing go-libp2p's order. Every implementation has
		// to support Noise and fewer support TLS, so offering it first usually
		// avoids a rejected proposal - a round trip a peer with a tight handshake
		// deadline may not have to spare.
		libp2p.Security(noise.ID, noise.New),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),

		// Only what the peers this dials actually offer. The defaults add
		// WebTransport and WebRTC, and WebRTC brings a media stack that inspects
		// network interfaces - which Android refuses an ordinary app - for a
		// transport no configured peer advertises.
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(quic.NewTransport),
		libp2p.Transport(websocket.New),
	}

	if config.resolver != nil {
		opts = append(opts, libp2p.MultiaddrResolver(config.resolver))
	}

	return libp2p.New(opts...)
}

// listenOn opens a listener only when the caller asked for one. Nothing dials
// this node: it fetches, the DHT runs as a client, and the address it would
// advertise is a loopback one nobody can reach.
//
// Not listening also keeps libp2p from enumerating network interfaces, which it
// does on a timer for as long as it has an address to maintain. Android refuses
// that to an ordinary app, so every pass is a denial in the system log and an
// error from libp2p.
func listenOn(port int32) libp2p.Option {
	if port <= 0 {
		return libp2p.NoListenAddrs
	}

	return libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port))
}

// parsePeers validates bootstrap addresses when the Client is built rather than
// at the first download. Invalid entries are skipped, and an empty result is not
// an error: gateways or an indexer may be the only configured route.
func parsePeers(addrs []string) []peer.AddrInfo {
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

	// Configured order rather than the map's, so behaviour is stable.
	peers := make([]peer.AddrInfo, 0, len(order))
	for _, id := range order {
		peers = append(peers, *infos[id])
	}

	return peers
}

// dialGroup is a set of dials running in the background.
type dialGroup struct {
	// first closes when one target has answered, done when all have settled.
	first chan struct{}
	done  chan struct{}

	connected atomic.Int32
}

func (group *dialGroup) count() int {
	return int(group.connected.Load())
}

// finished reports whether every dial has settled, successfully or not.
func (group *dialGroup) finished() bool {
	select {
	case <-group.done:
		return true
	default:
		return false
	}
}

// dialAll starts every dial at once and returns without waiting for any of them.
// Dials still running when the caller moves on keep running, so a target that
// answers late still joins the exchange and can serve blocks mid-download.
func dialAll(
	ctx context.Context,
	targets []peer.AddrInfo,
	connect func(context.Context, peer.AddrInfo) error,
	name func(peer.AddrInfo) string,
) *dialGroup {
	group := &dialGroup{first: make(chan struct{}), done: make(chan struct{})}

	if len(targets) == 0 {
		close(group.done)
		return group
	}

	var (
		wait sync.WaitGroup
		once sync.Once
	)

	wait.Add(len(targets))

	for _, target := range targets {
		go func() {
			defer wait.Done()

			if err := connect(ctx, target); err != nil {
				reportOnce(name(target), err)
				return
			}

			group.connected.Add(1)
			once.Do(func() { close(group.first) })
		}()
	}

	go func() {
		wait.Wait()
		close(group.done)
	}()

	return group
}

// Targets already reported as unreachable. A gateway that cannot serve verified
// blocks fails on every node start, and a node starts whenever the client has
// been idle, so saying it once keeps a standing condition from reading as a
// recurring incident. Keyed on the target, since the message varies between
// attempts.
var reported sync.Map

func reportOnce(subject string, err error) {
	if _, seen := reported.LoadOrStore(subject, struct{}{}); seen {
		return
	}

	fmt.Printf("failed to connect to %s: %s\n", subject, err)
}

// gatewayName renders a gateway for logs, where its synthetic peer ID would be
// noise. Connecting to one means it responded to a probe, not that a libp2p
// handshake completed.
func gatewayName(gateway peer.AddrInfo) string {
	if len(gateway.Addrs) == 0 {
		return "gateway " + gateway.ID.String()
	}

	return "gateway " + gateway.Addrs[0].String()
}

func peerName(info peer.AddrInfo) string {
	return "peer " + info.ID.String()
}
