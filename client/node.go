package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"

	"github.com/libp2p/go-libp2p"
	routinghelpers "github.com/libp2p/go-libp2p-routing-helpers"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/multiformats/go-multiaddr"

	bsclient "github.com/ipfs/boxo/bitswap/client"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/ipld/merkledag"
	unixfile "github.com/ipfs/boxo/ipld/unixfs/file"
)

type Node interface {
	Download(ctx context.Context, cidStr string, output string, sizeLimit int64) error
	Connect()
	Close()
}

type NodeConcrete struct {
	id        string
	cancel    context.CancelFunc
	connected int

	host   host.Host
	client *bsclient.Client
}

type NodeConfig struct {
	BootstrapPeers []string
	Port           int32
}

func (node *NodeConcrete) Download(ctx context.Context, cidStr string, output string, sizeLimit int64) error {
	parsed, err := cid.Parse(cidStr)
	if err != nil {
		return fmt.Errorf("invalid cid %q: %w", cidStr, err)
	}

	bserv := blockservice.New(blockstore.NewBlockstore(datastore.NewNullDatastore()), node.client)
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

	// A download that hits the deadline is abandoned part way through the write,
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

func (node *NodeConcrete) Connect() {
	node.connected++
}

func (node *NodeConcrete) Close() {
	if !node.release() {
		return
	}

	// host.Close waits on the host's background goroutines and client.Close
	// waits for bitswap to shut down. Both run outside nodeMutex, or every
	// concurrent GetNode stalls behind them with no deadline of its own.
	node.cancel()
	node.client.Close()
	node.host.Close()
}

// release drops one reference to the node and reports whether it was the last
// one. When it was, the node has already been removed from the registry - so a
// concurrent GetNode starts a fresh one rather than waiting on this shutdown -
// and the caller owns tearing it down.
func (node *NodeConcrete) release() bool {
	nodeMutex.Lock()
	defer nodeMutex.Unlock()

	node.connected--
	if node.connected > 0 {
		return false
	}

	delete(nodes, node.id)

	return true
}

var (
	nodes     = make(map[string]Node)
	nodeMutex sync.Mutex
)

func GetNode(config *NodeConfig) (Node, error) {
	id := getNodeId(config)

	nodeMutex.Lock()
	defer nodeMutex.Unlock()

	if node, exists := nodes[id]; exists {
		node.Connect()
		return node, nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	host, client, err := startNode(ctx, config)
	if err != nil {
		cancel()
		return nil, err
	}

	go func() {
		err := connectToPeers(ctx, host, config.BootstrapPeers)
		if err != nil {
			fmt.Printf("failed to connect to peers: %s\n", err)
		}
	}()

	node := &NodeConcrete{id, cancel, 1, host, client}

	nodes[id] = node
	return node, nil
}

func getNodeId(config *NodeConfig) string {
	sort.Strings(config.BootstrapPeers)

	hash := sha256.New()
	hash.Write([]byte(fmt.Sprintf("%v", config)))

	return hex.EncodeToString(hash.Sum(nil))
}

func startNode(ctx context.Context, config *NodeConfig) (host.Host, *bsclient.Client, error) {
	host, err := makeHost(config.Port)
	if err != nil {
		return nil, nil, err
	}

	client := startClient(ctx, host)

	return host, client, nil
}

func makeHost(port int32) (host.Host, error) {
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

func startClient(ctx context.Context, host host.Host) *bsclient.Client {
	network := bsnet.NewFromIpfsHost(host, routinghelpers.Null{})
	client := bsclient.New(ctx, network, blockstore.NewBlockstore(datastore.NewNullDatastore()))
	network.Start(client)

	return client
}

func connectToPeers(ctx context.Context, host host.Host, peers []string) error {
	if len(peers) == 0 {
		return fmt.Errorf("no bootstrap peers configured, there is nobody to fetch blocks from")
	}

	var wg sync.WaitGroup
	peerInfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			fmt.Printf("skipping invalid bootstrap peer %q: %s\n", addrStr, err)
			continue
		}

		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			fmt.Printf("skipping invalid bootstrap peer %q: %s\n", addrStr, err)
			continue
		}

		pi, ok := peerInfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			peerInfos[pi.ID] = pi
		}

		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}

	if len(peerInfos) == 0 {
		return fmt.Errorf("none of the %d configured bootstrap peers is a valid address", len(peers))
	}

	var connected atomic.Int32

	wg.Add(len(peerInfos))
	for _, peerInfo := range peerInfos {
		go func(peerInfo *peer.AddrInfo) {
			defer wg.Done()
			err := host.Connect(ctx, *peerInfo)
			if err != nil {
				fmt.Printf("failed to connect to %s: %s\n", peerInfo.ID, err)
				return
			}
			connected.Add(1)
		}(peerInfo)
	}
	wg.Wait()

	if connected.Load() == 0 {
		return fmt.Errorf("failed to connect to any of the %d bootstrap peers", len(peerInfos))
	}

	return nil
}
