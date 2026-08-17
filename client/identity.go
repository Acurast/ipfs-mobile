package client

import (
	"crypto/ed25519"
	"fmt"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// SeedSize is how many bytes Config.IdentitySeed takes.
const SeedSize = ed25519.SeedSize

// nodeIdentity returns the key the host is known by.
//
// A seed makes the peer id the same on every start, so a node that comes back
// after an idle shutdown is the one peers already met rather than a stranger.
// Without one the identity lasts only as long as the node.
//
// Ed25519 either way, since RSA keygen costs seconds on mobile ARM cores.
func nodeIdentity(seed []byte) (crypto.PrivKey, error) {
	if err := checkIdentitySeed(seed); err != nil {
		return nil, err
	}

	if len(seed) == 0 {
		priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)

		return priv, err
	}

	return crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(seed))
}

// checkIdentitySeed reports whether a seed can name an identity, so that New can
// refuse one before a download is waiting on it. Empty passes and asks for an
// identity that lasts only as long as the node.
//
// A wrong length is refused rather than padded or truncated, either of which
// would hand back a peer id nobody asked for.
func checkIdentitySeed(seed []byte) error {
	if len(seed) == 0 || len(seed) == SeedSize {
		return nil
	}

	return fmt.Errorf("identity seed is %d bytes, want %d", len(seed), SeedSize)
}
