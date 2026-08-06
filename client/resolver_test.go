package client

import (
	"context"
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

// A configured server is the one asked, rather than whatever Go read from a
// resolver configuration that Android does not publish.
func TestResolverQueriesTheConfiguredServer(t *testing.T) {
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
