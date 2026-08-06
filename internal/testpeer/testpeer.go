// Package testpeer provides in-process IPFS peers for tests: real libp2p hosts
// serving real UnixFS DAGs over a real exchange, with only the network hop local.
// A mock would hide the libp2p, bitswap and routing behaviour these tests exist
// to pin down.
//
// Imported only from _test.go files, so it is never part of a gomobile bind.
package testpeer

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	chunker "github.com/ipfs/boxo/chunker"
	offline "github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	importer "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	pb "github.com/ipfs/boxo/ipld/unixfs/pb"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	multihash "github.com/multiformats/go-multihash"
	"google.golang.org/protobuf/proto"
)

// UnreachableCID is well formed but served by nobody, so a download of it blocks
// until its deadline.
const UnreachableCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"

// DeadPeer is a valid multiaddr for a peer that is not listening. Dialling it is
// refused immediately.
const DeadPeer = "/ip4/127.0.0.1/tcp/1/p2p/12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

const setupTimeout = 30 * time.Second

// Serve starts a peer holding content and returns the multiaddr to bootstrap
// from together with the root CID. Torn down when the test ends.
//
// It runs a DHT in server mode as well, since clients under test wait for their
// routing table to fill and would otherwise sit through that wait every time.
func Serve(t *testing.T, content []byte) (addr string, root string) {
	t.Helper()

	host, _ := startPeer(t)
	root = serveContent(t, host, content)

	return peerAddr(t, host), root
}

// ServeViaDHT starts a bootstrap node holding no content and a provider that
// holds it and announces it to the DHT. The returned address is the bootstrap
// node, so the only route to the content is a provider lookup.
func ServeViaDHT(t *testing.T, content []byte) (bootstrapAddr string, root string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	bootstrapHost, _ := startPeer(t)
	providerHost, providerDHT := startPeer(t)
	root = serveContent(t, providerHost, content)

	if err := providerHost.Connect(ctx, *host.InfoFromHost(bootstrapHost)); err != nil {
		t.Fatalf("joining the provider to the bootstrap node: %v", err)
	}

	if err := providerDHT.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrapping the provider dht: %v", err)
	}
	waitForRoutingTable(t, providerDHT)

	parsed, err := cid.Parse(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerDHT.Provide(ctx, parsed, true); err != nil {
		t.Fatalf("announcing content to the dht: %v", err)
	}

	return peerAddr(t, bootstrapHost), root
}

// ServeIsolated starts a bootstrap node holding no content and a provider that
// holds it but announces it nowhere. A client given only the bootstrap address
// cannot reach it unaided, which is what makes this the fixture for delegated
// routing and the control for having no routing at all.
func ServeIsolated(t *testing.T, content []byte) (bootstrapAddr string, providerAddr string, root string) {
	t.Helper()

	bootstrapHost, _ := startPeer(t)

	providerHost, _ := startPeer(t)
	root = serveContent(t, providerHost, content)

	return peerAddr(t, bootstrapHost), peerAddr(t, providerHost), root
}

// ServeTrustlessGateway starts an HTTP gateway answering the block requests a
// verified fetch makes, and counts them so a test can prove retrieval went
// through the gateway.
func ServeTrustlessGateway(t *testing.T, content []byte) (gatewayURL string, root string, blockRequests *atomic.Int32) {
	t.Helper()

	store := blockstore.NewBlockstore(dssync.MutexWrap(datastore.NewMapDatastore()))
	rootCid := addUnixfsFile(t, store, content)
	blockRequests = &atomic.Int32{}

	handler := http.NewServeMux()
	handler.HandleFunc("/ipfs/", func(w http.ResponseWriter, r *http.Request) {
		requested := strings.TrimPrefix(r.URL.Path, "/ipfs/")

		parsed, err := cid.Parse(requested)
		if err != nil {
			http.Error(w, "bad cid", http.StatusBadRequest)
			return
		}

		// httpnet probes for liveness with the identity CID before using an
		// endpoint. Identity CIDs inline their content, so answer from the
		// multihash; a 404 here means the gateway is never used at all.
		if decoded, err := multihash.Decode(parsed.Hash()); err == nil && decoded.Code == multihash.IDENTITY {
			w.Header().Set("Content-Type", "application/vnd.ipld.raw")
			w.Write(decoded.Digest)
			return
		}

		block, err := store.Get(r.Context(), parsed)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		if r.URL.Query().Get("format") == "raw" || strings.Contains(r.Header.Get("Accept"), "application/vnd.ipld.raw") {
			blockRequests.Add(1)
			w.Header().Set("Content-Type", "application/vnd.ipld.raw")
			w.Write(block.RawData())
			return
		}

		http.Error(w, "only raw blocks served here", http.StatusNotAcceptable)
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL, rootCid.String(), blockRequests
}

// ServeWholeFileGateway starts an HTTP gateway that serves content as a tar
// archive but refuses block requests, which is how a gateway without trustless
// support behaves. servedBody may deliberately not match the CID.
func ServeWholeFileGateway(t *testing.T, root string, servedBody []byte) (gatewayURL string, requests *atomic.Int32) {
	t.Helper()

	requests = &atomic.Int32{}

	handler := http.NewServeMux()
	handler.HandleFunc("/ipfs/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "raw" || strings.Contains(r.Header.Get("Accept"), "application/vnd.ipld.raw") {
			http.Error(w, "no trustless support", http.StatusNotAcceptable)
			return
		}

		if strings.TrimPrefix(r.URL.Path, "/ipfs/") != root {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		requests.Add(1)
		w.Header().Set("Content-Type", "application/x-tar")

		archive := tar.NewWriter(w)
		defer archive.Close()

		header := &tar.Header{
			Name:     root,
			Mode:     0o644,
			Size:     int64(len(servedBody)),
			Typeflag: tar.TypeReg,
		}
		if err := archive.WriteHeader(header); err != nil {
			return
		}
		archive.Write(servedBody)
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL, requests
}

// Stalled returns the multiaddr of a listener that accepts TCP connections and
// then says nothing, so libp2p hangs in its security handshake until the
// caller's deadline expires. A refused connection fails too fast to be useful.
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

			// Held open, never written to.
			accepted <- conn
		}
	}()

	// One cleanup, in this order: closing the listener ends the accept loop and
	// closes the channel, so draining first would wait forever.
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

	// A small chunk size so even modest fixtures produce a multi-block DAG.
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

// ServeSymlink serves a DAG whose root is a UnixFS symlink pointing at target.
func ServeSymlink(t *testing.T, target string) (addr string, root string) {
	t.Helper()

	symlink := unixfsSymlink(t, target)

	return serveNodes(t, symlink.Cid(), symlink), symlink.Cid().String()
}

// ServeDirectoryWithSymlink serves a directory holding one regular file and one
// symlink pointing at target.
func ServeDirectoryWithSymlink(t *testing.T, target string) (addr string, root string) {
	t.Helper()

	symlink := unixfsSymlink(t, target)

	contents := merkledag.NodeWithData(unixfsBytes(t, pb.Data_File, []byte("harmless")))

	dirNode := unixfs.EmptyDirNode()
	if err := dirNode.AddNodeLink("harmless.txt", contents); err != nil {
		t.Fatal(err)
	}
	if err := dirNode.AddNodeLink("link", symlink); err != nil {
		t.Fatal(err)
	}

	return serveNodes(t, dirNode.Cid(), dirNode, contents, symlink), dirNode.Cid().String()
}

// ServeUnderdeclaredFile serves a file DAG that declares `declared` bytes while
// linking `leaves` raw leaves of `leafSize` bytes each. Nothing in UnixFS
// reconciles the two.
func ServeUnderdeclaredFile(t *testing.T, declared uint64, leaves int, leafSize int) (addr string, root string) {
	t.Helper()

	fsnode := unixfs.NewFSNode(pb.Data_File)

	nodes := make([]format.Node, 0, leaves+1)
	rootNode := merkledag.NodeWithData(nil)

	for i := range leaves {
		leaf := merkledag.NewRawNode(bytes.Repeat([]byte{byte('a' + i%26)}, leafSize))
		nodes = append(nodes, leaf)

		if err := rootNode.AddNodeLink("", leaf); err != nil {
			t.Fatal(err)
		}
		fsnode.AddBlockSize(uint64(leafSize))
	}

	encoded, err := fsnode.GetBytes()
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the declared filesize after the blocksizes are in place, so the
	// root disagrees with its own leaves.
	encoded = withDeclaredFileSize(t, encoded, declared)
	rootNode.SetData(encoded)

	nodes = append(nodes, rootNode)

	return serveNodes(t, rootNode.Cid(), nodes...), rootNode.Cid().String()
}

func unixfsSymlink(t *testing.T, target string) format.Node {
	t.Helper()

	return merkledag.NodeWithData(unixfsBytes(t, pb.Data_Symlink, []byte(target)))
}

func unixfsBytes(t *testing.T, kind pb.Data_DataType, data []byte) []byte {
	t.Helper()

	fsnode := unixfs.NewFSNode(kind)
	fsnode.SetData(data)

	encoded, err := fsnode.GetBytes()
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}

// withDeclaredFileSize re-encodes the unixfs protobuf with filesize replaced.
func withDeclaredFileSize(t *testing.T, encoded []byte, size uint64) []byte {
	t.Helper()

	var data pb.Data
	if err := proto.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}

	data.Filesize = &size

	rewritten, err := proto.Marshal(&data)
	if err != nil {
		t.Fatal(err)
	}

	return rewritten
}

// serveNodes starts a peer whose blockstore holds exactly nodes.
func serveNodes(t *testing.T, root cid.Cid, nodes ...format.Node) (addr string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	host, _ := startPeer(t)

	store := blockstore.NewBlockstore(dssync.MutexWrap(datastore.NewMapDatastore()))
	for _, node := range nodes {
		if err := store.Put(ctx, node); err != nil {
			t.Fatal(err)
		}
	}

	network := bsnet.NewFromIpfsHost(host)
	server := bitswap.New(ctx, network, nil, store)
	t.Cleanup(func() { server.Close() })

	return peerAddr(t, host)
}
