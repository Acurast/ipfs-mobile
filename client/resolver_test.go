package client

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	ma "github.com/multiformats/go-multiaddr"
)

func TestDNSServerAddr(t *testing.T) {
	for _, test := range []struct {
		server string
		want   string
	}{
		{"1.1.1.1", "1.1.1.1:53"},
		{"1.1.1.1:5353", "1.1.1.1:5353"},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]:5353", "[2606:4700:4700::1111]:5353"},
		// Link local resolvers are reported with the interface they belong to.
		{"fe80::1%wlan0", "[fe80::1%wlan0]:53"},
	} {
		got, err := dnsServerAddr(test.server)
		if err != nil {
			t.Errorf("dnsServerAddr(%q): %v", test.server, err)
			continue
		}
		if got != test.want {
			t.Errorf("dnsServerAddr(%q) = %q, want %q", test.server, got, test.want)
		}
	}

	for _, server := range []string{"", "not-an-address", "example.com"} {
		if _, err := dnsServerAddr(server); err == nil {
			t.Errorf("dnsServerAddr(%q) was accepted", server)
		}
	}
}

func TestNewResolverWithoutServersLeavesLibp2pItsOwn(t *testing.T) {
	resolver, err := newResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		t.Error("a resolver was built with no servers configured")
	}
}

func TestNewResolverRejectsServersItCannotUse(t *testing.T) {
	if _, err := newResolver([]string{"not-an-address"}); err == nil {
		t.Fatal("an unusable dns server was accepted")
	}
}

// forgetTXTAnswers empties the process-wide cache, since a test that expects a
// lookup to reach a server must not be answered by what another one cached.
func forgetTXTAnswers(t *testing.T) {
	t.Helper()

	txtAnswers = newTXTCache()
	t.Cleanup(func() { txtAnswers = newTXTCache() })
}

// A configured server is the one asked, rather than whatever Go read from a
// resolver configuration that Android does not publish.
func TestResolverQueriesTheConfiguredServer(t *testing.T) {
	forgetTXTAnswers(t)

	server, queried := stubDNS(t)

	resolver, err := newResolver([]string{server})
	if err != nil {
		t.Fatal(err)
	}
	if resolver == nil {
		t.Fatal("no resolver was built")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addr := ma.StringCast("/dnsaddr/example.invalid/p2p/" + testPeerID)

	// The stub never answers, so this only returns at the deadline. Being asked is
	// the whole assertion, and that happens long before then.
	go resolver.ResolveDNSAddr(ctx, "", addr, 1, 8)

	select {
	case name := <-queried:
		if !strings.Contains(name, "example.invalid") {
			t.Errorf("the server was asked for %q, want the dnsaddr name", name)
		}
	case <-ctx.Done():
		t.Fatal("the configured dns server was never asked")
	}
}

const testPeerID = "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

// stubDNS listens for one UDP query and reports the name it was asked for. It
// never answers, since only the fact that it was reached is under test.
func stubDNS(t *testing.T) (server string, queried chan string) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting the stub resolver: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	queried = make(chan string, 4)

	go func() {
		buf := make([]byte, 512)

		for {
			count, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}

			select {
			case queried <- questionName(buf[:count]):
			default:
			}
		}
	}()

	return conn.LocalAddr().String(), queried
}

// questionName reads the name out of a DNS query, whose question section starts
// after the twelve byte header and holds length prefixed labels.
func questionName(query []byte) string {
	const header = 12

	if len(query) <= header {
		return ""
	}

	var (
		labels []string
		at     = header
	)

	for at < len(query) {
		length := int(query[at])
		if length == 0 {
			break
		}

		at++
		if at+length > len(query) {
			break
		}

		labels = append(labels, string(query[at:at+length]))
		at += length
	}

	return strings.Join(labels, ".")
}

// The bootstrap names are looked up once rather than on every dial, which is what
// keeps them off the wire for the life of a node.
func TestTXTAnswersAreReused(t *testing.T) {
	forgetTXTAnswers(t)

	server, queried := answeringStubDNS(t, "dnsaddr=/ip4/104.131.131.82/tcp/4001/p2p/"+testPeerID)

	resolver, err := newResolver([]string{server})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A name of its own, so what this test caches cannot answer a lookup another
	// one expects to reach a server.
	addr := ma.StringCast("/dnsaddr/cached.invalid/p2p/" + testPeerID)

	if _, err := resolver.ResolveDNSAddr(ctx, "", addr, 1, 8); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	asked := len(queried)
	if asked == 0 {
		t.Fatal("the first lookup never reached the server")
	}

	if _, err := resolver.ResolveDNSAddr(ctx, "", addr, 1, 8); err != nil {
		t.Fatalf("second lookup: %v", err)
	}

	if again := len(queried) - asked; again != 0 {
		t.Errorf("the second lookup sent %d more queries, want it answered from the cache", again)
	}
}

func TestTXTCacheForgetsLapsedAnswers(t *testing.T) {
	cache := newTXTCache()

	cache.keep("fresh", []string{"a"})
	if _, held := cache.lookup("fresh"); !held {
		t.Error("an answer just stored was not held")
	}

	cache.answers["stale"] = txtAnswer{records: []string{"b"}, until: time.Now().Add(-time.Second)}
	if _, held := cache.lookup("stale"); held {
		t.Error("a lapsed answer was served")
	}
}

// The names come out of records a peer serves, so the cache cannot grow with them.
func TestTXTCacheIsBounded(t *testing.T) {
	cache := newTXTCache()

	for i := range txtAnswerLimit * 4 {
		cache.keep(fmt.Sprintf("name-%d", i), []string{"a"})
	}

	if held := len(cache.answers); held > txtAnswerLimit {
		t.Errorf("the cache holds %d answers, want at most %d", held, txtAnswerLimit)
	}
}

// answeringStubDNS answers every query with one TXT record, which a lookup has to
// succeed at before there is anything to cache. stubDNS never answers.
func answeringStubDNS(t *testing.T, record string) (server string, queried chan string) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting the stub resolver: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	queried = make(chan string, 16)

	go func() {
		buf := make([]byte, 512)

		for {
			count, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}

			query := make([]byte, count)
			copy(query, buf[:count])

			select {
			case queried <- questionName(query):
			default:
			}

			if answer := txtResponse(query, record); answer != nil {
				conn.WriteTo(answer, from)
			}
		}
	}()

	return conn.LocalAddr().String(), queried
}

// txtResponse answers query with a single TXT record. The question is echoed back
// ahead of the answer, and the answer names it by pointing at that copy.
func txtResponse(query []byte, record string) []byte {
	const header = 12

	if len(query) < header || len(record) > 255 {
		return nil
	}

	// Past the labels of the question, then its terminator, type and class.
	end := header
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}
	end += 5

	if end > len(query) {
		return nil
	}

	answer := make([]byte, end, end+12+len(record))
	copy(answer, query[:end])

	answer[2], answer[3] = 0x81, 0x80 // answered, without error
	answer[6], answer[7] = 0, 1       // one answer follows
	answer[8], answer[9] = 0, 0
	answer[10], answer[11] = 0, 0

	answer = append(answer,
		0xc0, header, // the name, as a pointer to the question
		0, 16, // TXT
		0, 1, // IN
		0, 0, 0, 60, // a minute to live
		0, byte(len(record)+1), // the record, and its own length prefix
		byte(len(record)),
	)

	return append(answer, record...)
}

// Host lookups stay with the platform; only TXT is diverted.
func TestOnlyTXTLookupsUseTheConfiguredServers(t *testing.T) {
	server, queried := stubDNS(t)

	resolver, err := newResolver([]string{server})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolver.ResolveDNSComponent(ctx, ma.StringCast("/dns4/example.invalid"), 8)

	select {
	case name := <-queried:
		t.Errorf("a host lookup was sent to the configured server (asked for %q)", name)
	case <-time.After(2 * time.Second):
	}
}
