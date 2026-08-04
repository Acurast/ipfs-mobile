// Package testpeer provides in-process IPFS peers for tests.
//
// The peers here are real libp2p hosts serving real UnixFS DAGs over a real
// bitswap exchange; only the network hop is local. Substituting a mock for the
// node would hide the behaviour these tests exist to pin down, since the bugs
// this package was written for lived in the real libp2p and bitswap shutdown
// and dial paths rather than in any logic of ours.
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

	"github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network"
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
	routinghelpers "github.com/libp2p/go-libp2p-routing-helpers"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// UnreachableCID is well formed but served by nobody, so a download of it blocks
// until its deadline.
const UnreachableCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"

// DeadPeer is a valid multiaddr for a peer that is not listening. Dialling it is
// refused immediately.
const DeadPeer = "/ip4/127.0.0.1/tcp/1/p2p/12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

// Serve starts a bitswap server holding content and returns the multiaddr to
// bootstrap from together with the root CID. Everything is torn down when the
// test finishes.
func Serve(t *testing.T, content []byte) (addr string, root string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("generating peer identity: %v", err)
	}

	host, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.Identity(priv),
	)
	if err != nil {
		t.Fatalf("starting peer host: %v", err)
	}
	t.Cleanup(func() { host.Close() })

	store := blockstore.NewBlockstore(dssync.MutexWrap(datastore.NewMapDatastore()))
	rootCid := addUnixfsFile(t, store, content)

	network := bsnet.NewFromIpfsHost(host, routinghelpers.Null{})
	server := bitswap.New(ctx, network, store)
	t.Cleanup(func() { server.Close() })

	addrs := host.Addrs()
	if len(addrs) == 0 {
		t.Fatal("peer host is not listening on any address")
	}

	return fmt.Sprintf("%s/p2p/%s", addrs[0], host.ID()), rootCid.String()
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
