// Package testpeer provides in-process IPFS peers for tests.
//
// The peers here are real libp2p hosts serving real UnixFS DAGs over a real
// bitswap exchange, with a real DHT; only the network hop is local. Substituting
// a mock for the node would hide the behaviour these tests exist to pin down,
// since the bugs this package was written for lived in the real libp2p, bitswap
// and routing paths rather than in any logic of ours.
//
// It lives under internal/ so it can be shared by the client and ffi tests
// without becoming part of the published surface, and it is imported only from
// _test.go files, so it is never part of a gomobile bind.
package testpeer

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	chunker "github.com/ipfs/boxo/chunker"
	offline "github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	importer "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// UnreachableCID is well formed but served by nobody, so a download of it blocks
// until its deadline.
const UnreachableCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"

// DeadPeer is a valid multiaddr for a peer that is not listening. Dialling it is
// refused immediately.
const DeadPeer = "/ip4/127.0.0.1/tcp/1/p2p/12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

const setupTimeout = 30 * time.Second

// Serve starts a peer holding content and returns the multiaddr to bootstrap
// from together with the root CID. Everything is torn down when the test ends.
//
// The peer runs a DHT in server mode as well as a bitswap server. Clients under
// test run a DHT in client mode and wait for their routing table to fill before
// downloading, so without a DHT server to talk to every test would sit through
// that wait.
func Serve(t *testing.T, content []byte) (addr string, root string) {
	t.Helper()

	host, _ := startPeer(t)
	root = serveContent(t, host, content)

	return peerAddr(t, host), root
}

// ServeViaDHT starts two peers: a bootstrap node holding no content, and a
// separate provider that holds the content and announces it to the DHT. The
// returned address is the bootstrap node.
//
// A client given only that address cannot reach the content by direct
// connection, because the node it connects to does not have it. The only route
// is a DHT provider lookup, which is what makes this a test of content routing
// rather than of bitswap alone.
func ServeViaDHT(t *testing.T, content []byte) (bootstrapAddr string, root string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	// Knows every peer, holds no blocks.
	bootstrapHost, _ := startPeer(t)

	// Holds the blocks, reachable only once discovered.
	providerHost, providerDHT := startPeer(t)
	root = serveContent(t, providerHost, content)

	if err := providerHost.Connect(ctx, *host.InfoFromHost(bootstrapHost)); err != nil {
		t.Fatalf("joining the provider to the bootstrap node: %v", err)
	}

	if err := providerDHT.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrapping the provider dht: %v", err)
	}
	waitForRoutingTable(t, providerDHT)

	// Publishes a provider record for root, which is what the client will look up.
	parsed, err := cid.Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerDHT.Provide(ctx, parsed, true); err != nil {
		t.Fatalf("announcing content to the dht: %v", err)
	}

	return peerAddr(t, bootstrapHost), root
}

// ServeIsolated starts two peers: a bootstrap node holding no content, and a
// provider that holds it but announces it nowhere at all - no DHT record, no
// index entry. Returns both addresses and the root CID.
//
// A client given only the bootstrap address cannot reach the content by any
// means of its own; something has to tell it where the provider is. That is what
// makes this the right fixture for testing delegated routing, and the control
// for testing that no routing means no retrieval.
func ServeIsolated(t *testing.T, content []byte) (bootstrapAddr string, providerAddr string, root string) {
	t.Helper()

	bootstrapHost, _ := startPeer(t)

	providerHost, _ := startPeer(t)
	root = serveContent(t, providerHost, content)

	return peerAddr(t, bootstrapHost), peerAddr(t, providerHost), root
}

// Stalled returns the multiaddr of a listener that accepts TCP connections and
// then says nothing, so libp2p completes the dial and hangs in its security
// handshake until the caller's deadline expires.
//
// A refused connection fails in microseconds, which is useless for testing that
// node startup runs under the deadline; this stalls instead.
func Stalled(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting stalled listener: %v", err)
	}

	accepted := make(chan net.Conn, 16)
	go func() {
		defer close(accepted)

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			// Hold the connection open without ever writing to it.
			accepted <- conn
		}
	}()

	// One cleanup, in this order: closing the listener is what ends the accept
	// loop and closes the channel, so draining first would wait forever.
	t.Cleanup(func() {
		listener.Close()

		for conn := range accepted {
			conn.Close()
		}
	})

	_, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("generating stalled peer identity: %v", err)
	}

	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("deriving stalled peer id: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port

	return fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", port, id)
}

// Content returns deterministic bytes of the requested size.
func Content(size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte('a' + i%26)
	}

	return content
}

// startPeer brings up a host with a DHT in server mode, so peers under test can
// both discover through it and populate a routing table from it.
func startPeer(t *testing.T) (host.Host, *dht.IpfsDHT) {
	t.Helper()

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("generating peer identity: %v", err)
	}

	peerHost, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.Identity(priv),
	)
	if err != nil {
		t.Fatalf("starting peer host: %v", err)
	}
	t.Cleanup(func() { peerHost.Close() })

	kad, err := dht.New(peerHost, dht.Mode(dht.ModeServer))
	if err != nil {
		peerHost.Close()
		t.Fatalf("starting peer dht: %v", err)
	}
	t.Cleanup(func() { kad.Close() })

	return peerHost, kad
}

// serveContent attaches a bitswap server backed by a blockstore holding content,
// and returns its root CID.
func serveContent(t *testing.T, peerHost host.Host, content []byte) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := blockstore.NewBlockstore(dssync.MutexWrap(datastore.NewMapDatastore()))
	root := addUnixfsFile(t, store, content)

	network := bsnet.NewFromIpfsHost(peerHost)
	server := bitswap.New(ctx, network, nil, store)
	t.Cleanup(func() { server.Close() })

	return root.String()
}

func peerAddr(t *testing.T, peerHost host.Host) string {
	t.Helper()

	addrs := peerHost.Addrs()
	if len(addrs) == 0 {
		t.Fatal("peer host is not listening on any address")
	}

	return fmt.Sprintf("%s/p2p/%s", addrs[0], peerHost.ID())
}

func waitForRoutingTable(t *testing.T, kad *dht.IpfsDHT) {
	t.Helper()

	deadline := time.Now().Add(setupTimeout)
	for kad.RoutingTable().Size() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dht routing table never filled")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// addUnixfsFile writes content into store as a UnixFS DAG and returns its root.
func addUnixfsFile(t *testing.T, store blockstore.Blockstore, content []byte) cid.Cid {
	t.Helper()

	dserv := merkledag.NewDAGService(blockservice.New(store, offline.Exchange(store)))

	prefix, err := merkledag.PrefixForCidVersion(1)
	if err != nil {
		t.Fatalf("building cid prefix: %v", err)
	}

	params := importer.DagBuilderParams{
		Maxlinks:   importer.DefaultLinksPerBlock,
		RawLeaves:  true,
		CidBuilder: &prefix,
		Dagserv:    dserv,
	}

	// A small chunk size so even modest fixtures produce a multi-block DAG, which
	// exercises the session and block-fetch paths rather than a single inlined
	// block.
	builder, err := params.New(chunker.NewSizeSplitter(bytes.NewReader(content), 1024))
	if err != nil {
		t.Fatalf("building dag: %v", err)
	}

	node, err := balanced.Layout(builder)
	if err != nil {
		t.Fatalf("laying out dag: %v", err)
	}

	return node.Cid()
}
