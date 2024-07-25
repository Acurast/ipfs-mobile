package client

import (
	"context"
	"fmt"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-routing-helpers"
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
	"github.com/ipfs/boxo/ipld/unixfs/file"
)

type Node interface {
	Close()
	ConnectToPeers(ctx context.Context, peers []string) error
	Download(ctx context.Context, cidStr string, output string) error
}

type NodeConcrete struct {
	host host.Host
	client *bsclient.Client
}

type NodeConfig struct {
	BootstrapPeers []string
	Port		   int32
}

func (node NodeConcrete) Close() {
	node.host.Close()
	node.client.Close()
}

func (node NodeConcrete) ConnectToPeers(ctx context.Context, peers []string) error {
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
			err := node.host.Connect(ctx, *peerInfo)
			if err != nil {
				fmt.Printf("failed to connect to %s: %s", peerInfo.ID, err)
			}
		}(peerInfo)
	}
	wg.Wait()

	return nil
}

func (node NodeConcrete) Download(ctx context.Context, cidStr string, output string) error {
	bserv := blockservice.New(blockstore.NewBlockstore(datastore.NewNullDatastore()), node.client)
	session := merkledag.NewSession(ctx, merkledag.NewDAGService(bserv))
	dserv := merkledag.NewReadOnlyDagService(session)

	nd, err := dserv.Get(ctx, cid.MustParse(cidStr))
	if err != nil {
		return err
	}

	unixfsnd, err := unixfile.NewUnixfsFile(ctx, dserv, nd)
	if err != nil {
		return err
	}

	return files.WriteTo(unixfsnd, output)
}

func StartNode(ctx context.Context, config *NodeConfig) (Node, error) {
	host, err := makeHost(config.Port)
	if err != nil {
		return nil, err
	}

	client := startClient(ctx, host)

	return NodeConcrete{host, client}, nil
}

func makeHost(port int32) (host.Host, error) {
	priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
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
