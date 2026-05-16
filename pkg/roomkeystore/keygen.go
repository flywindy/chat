package roomkeystore

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
)

// GenerateKeyPair returns a fresh P-256 keypair for a room.
func GenerateKeyPair() (RoomKeyPair, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return RoomKeyPair{}, fmt.Errorf("generate P-256 key: %w", err)
	}
	return RoomKeyPair{
		PublicKey:  priv.PublicKey().Bytes(),
		PrivateKey: priv.Bytes(),
	}, nil
}
