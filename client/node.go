package client

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/core/coreiface"
	"github.com/ipfs/kubo/core/node/libp2p"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo"
	"github.com/ipfs/kubo/repo/fsrepo"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

type Node interface {
	iface.CoreAPI
	ConnectToPeers(ctx context.Context, peers []string) error
	Download(ctx context.Context, cidStr string, output string) error
}

type NodeDecorator struct {
	iface.CoreAPI
}

type NodeConfig struct {
	BootstrapPeers []string
	Plugins        string
	Repo           string
}

func (node NodeDecorator) ConnectToPeers(ctx context.Context, peers []string) error {
	var wg sync.WaitGroup
	peerInfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			return err
		}

		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return err
		}

		pi, ok := peerInfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			peerInfos[pi.ID] = pi
		}

		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}

	wg.Add(len(peerInfos))
	for _, peerInfo := range peerInfos {
		go func(peerInfo *peer.AddrInfo) {
			defer wg.Done()
			err := node.Swarm().Connect(ctx, *peerInfo)
			if err != nil {
				fmt.Printf("failed to connect to %s: %s", peerInfo.ID, err)
			}
		}(peerInfo)
	}
	wg.Wait()

	return nil
}

func (node NodeDecorator) Download(ctx context.Context, cidStr string, output string) error {
	path := path.FromCid(cid.MustParse(cidStr))
	file, err := node.Unixfs().Get(ctx, path)
	if err != nil {
		return err
	}

	return files.WriteTo(file, output)
}

func StartNode(ctx context.Context, config *NodeConfig) (Node, error) {
	err := setupPlugins(config.Plugins)
	if err != nil {
		return nil, err
	}

	repo, err := openRepo(config.Repo)
	if err != nil {
		return nil, err
	}

	node, err := createNode(ctx, repo)
	if err != nil {
		return nil, err
	}

	api, err := coreapi.NewCoreAPI(node)
	if err != nil {
		return nil, err
	}

	return NodeDecorator{api}, nil
}

func setupPlugins(path string) error {
	plugins, err := loader.NewPluginLoader(path)
	if err != nil {
		return fmt.Errorf("failed to load plugins: %s", err)
	}

	err = plugins.Initialize()
	if err != nil {
		return fmt.Errorf("failed to initialize plugins: %s", err)
	}

	err = plugins.Inject()
	if err != nil {
		return fmt.Errorf("failed to inject plugins: %s", err)
	}

	return nil
}

func openRepo(path string) (repo.Repo, error) {
	config, err := config.Init(io.Discard, 2048)
	if err != nil {
		return nil, err
	}

	err = fsrepo.Init(path, config)
	if err != nil {
		return nil, err
	}

	return fsrepo.Open(path)
}

func createNode(ctx context.Context, repo repo.Repo) (*core.IpfsNode, error) {
	config := &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTClientOption,
		Repo:    repo,
	}

	return core.NewNode(ctx, config)
}
