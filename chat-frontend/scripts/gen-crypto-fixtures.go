//go:build ignore

// gen-crypto-fixtures generates a known-plaintext encrypted message for
// chat-frontend's lib/roomcrypto round-trip tests. Run with:
//
//	go run chat-frontend/scripts/gen-crypto-fixtures.go > chat-frontend/test/fixtures/encrypted-message.json
//
// Commit the output. The fixture exists to lock the cross-language wire
// format: any change in the server encoder's AES-GCM parameters or wire
// shape MUST update this fixture together with the corresponding TS
// decoder.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hmchangw/chat/pkg/roomcrypto"
)

func main() {
	// Deterministic private key so the fixture is stable across runs.
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}
	const plaintext = "fixture plaintext — encoded by the Go server, decoded by chat-frontend"
	const version = 1
	const roomID = "fixture-room"

	// Construct an Encoder with a fixed-nonce reader so the ciphertext is
	// reproducible. Twelve-byte zero nonce is fine for a fixture (it's a
	// known-key test, not real traffic).
	enc := roomcrypto.NewEncoder(roomcrypto.WithRand(zeroReader{}))
	msg, err := enc.Encode(roomID, plaintext, priv, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}

	out := struct {
		PrivateKey string                       `json:"privateKey"`
		Plaintext  string                       `json:"plaintext"`
		Message    *roomcrypto.EncryptedMessage `json:"message"`
	}{
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
		Plaintext:  plaintext,
		Message:    msg,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
