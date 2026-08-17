package client

import (
	"bytes"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func testSeed(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, SeedSize)
}

// Pins that a seed is what decides the peer id, and that two of them disagree.
func TestASeedNamesTheSamePeerEveryTime(t *testing.T) {
	seed := testSeed(0x01)

	first, err := nodeIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}

	again, err := nodeIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}

	if !first.Equals(again) {
		t.Error("the same seed produced a different identity")
	}

	other, err := nodeIdentity(testSeed(0x02))
	if err != nil {
		t.Fatal(err)
	}

	if first.Equals(other) {
		t.Error("a different seed produced the same identity")
	}
}

func TestWithoutASeedTheIdentityIsNewEachTime(t *testing.T) {
	first, err := nodeIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}

	again, err := nodeIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}

	if first.Equals(again) {
		t.Error("two unseeded nodes share an identity")
	}
}

// Every length either side of the one that works is refused.
func TestASeedOfTheWrongLengthIsRefused(t *testing.T) {
	for _, length := range []int{1, SeedSize - 1, SeedSize + 1, 2 * SeedSize} {
		if _, err := nodeIdentity(bytes.Repeat([]byte{0x01}, length)); err == nil {
			t.Errorf("a %d byte seed was accepted", length)
		}
	}
}

// Pins that the refusal happens in New rather than at the first download.
func TestNewRefusesASeedOfTheWrongLength(t *testing.T) {
	_, err := New(&Config{
		Gateways:     []string{"https://gateway.example.net"},
		IdentitySeed: []byte{0x01},
	})
	if err == nil {
		t.Fatal("New accepted a seed of the wrong length")
	}

	client, err := New(&Config{
		Gateways:     []string{"https://gateway.example.net"},
		IdentitySeed: testSeed(0x03),
	})
	if err != nil {
		t.Fatalf("New refused a usable seed: %v", err)
	}
	client.Close()
}

// The seeded identity is the one the host actually runs as, rather than something
// only nodeIdentity agrees with.
func TestASeededNodeRunsAsThatPeer(t *testing.T) {
	seed := testSeed(0x04)

	want, err := nodeIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}

	expected, err := peer.IDFromPublicKey(want.GetPublic())
	if err != nil {
		t.Fatal(err)
	}

	host, err := makeHost(nodeConfig{identitySeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	if host.ID() != expected {
		t.Errorf("host is running as %s, want %s", host.ID(), expected)
	}
}
