package roomcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptedMessage_JSONRoundTrip(t *testing.T) {
	original := EncryptedMessage{
		Version:    7,
		Nonce:      []byte{4, 5, 6},
		Ciphertext: []byte{7, 8, 9},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"version":7`)
	assert.NotContains(t, string(data), "ephemeralPublicKey", "legacy field must not appear in JSON output")

	var decoded EncryptedMessage
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, original, decoded)
}

// failReader is an io.Reader that always returns an error.
type failReader struct{}

func (f *failReader) Read(_ []byte) (int, error) {
	return 0, errors.New("injected read failure")
}

func TestEncoder_Encode_NonDeterminism(t *testing.T) {
	priv := bytes.Repeat([]byte{0x33}, 32)
	enc := NewEncoder()

	a, err := enc.Encode("room-1", "same content", priv, 1)
	require.NoError(t, err)
	b, err := enc.Encode("room-1", "same content", priv, 1)
	require.NoError(t, err)

	assert.False(t, bytes.Equal(a.Nonce, b.Nonce), "nonces must differ")
	assert.False(t, bytes.Equal(a.Ciphertext, b.Ciphertext), "ciphertexts must differ")
}

func TestEncoder_Encode_HappyPath(t *testing.T) {
	// Use a fixed 32-byte private key (a P-256 scalar's worth of entropy).
	priv := make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i + 1)
	}

	enc := NewEncoder()
	got, err := enc.Encode("room-1", "hello", priv, 7)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Version)
	assert.Len(t, got.Nonce, 12)
	assert.NotEmpty(t, got.Ciphertext)
}

func TestEncoder_Encode_CacheHit(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAB}, 32)
	enc := NewEncoder()

	_, err := enc.Encode("room-1", "msg1", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-1", "msg2", priv, 1)
	require.NoError(t, err)

	assert.Equal(t, 1, enc.cacheLen(), "same (roomID, version) must not re-derive the AES key")
}

func TestEncoder_Encode_DistinctVersionsCacheSeparately(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAB}, 32)
	enc := NewEncoder()

	_, err := enc.Encode("room-1", "msg1", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-1", "msg2", priv, 2)
	require.NoError(t, err)

	assert.Equal(t, 2, enc.cacheLen(), "different versions must occupy distinct cache entries")
}

func TestEncoder_Encode_EvictsLowestVersion(t *testing.T) {
	priv := bytes.Repeat([]byte{0x42}, 32)
	enc := NewEncoder(WithMaxCacheEntries(2))

	_, err := enc.Encode("room-A", "a", priv, 1)
	require.NoError(t, err)
	_, err = enc.Encode("room-A", "b", priv, 2)
	require.NoError(t, err)
	_, err = enc.Encode("room-A", "c", priv, 3)
	require.NoError(t, err)

	assert.Equal(t, 2, enc.cacheLen(), "cache must not exceed max")
	// Encoding for version 1 again must miss the cache (we evicted it),
	// while versions 2 and 3 must hit.
	prevLen := enc.cacheLen()
	_, err = enc.Encode("room-A", "d", priv, 2)
	require.NoError(t, err)
	assert.Equal(t, prevLen, enc.cacheLen(), "version 2 must still be cached (hit)")

	_, err = enc.Encode("room-A", "e", priv, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, enc.cacheLen(), "after re-inserting v=1, cache stays at max (now v=2 should have been evicted as lowest)")
}

func TestEncoder_Encode_NonceReaderError(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAA}, 32)
	enc := NewEncoder(WithRand(&failReader{}))

	got, err := enc.Encode("room-1", "hello", priv, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generating nonce")
	assert.Nil(t, got)
}

func TestEncoder_Encode_InvalidKeyLength(t *testing.T) {
	enc := NewEncoder()
	got, err := enc.Encode("room-1", "hello", make([]byte, 31), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be 32 bytes")
	assert.Nil(t, got)
}

func TestEncoder_Encode_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "non-empty", content: "hello, world"},
		{name: "empty", content: ""},
		{name: "unicode", content: "héllo 🌎"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priv := bytes.Repeat([]byte{0x55}, 32)
			enc := NewEncoder()
			msg, err := enc.Encode("room-1", tc.content, priv, 3)
			require.NoError(t, err)
			require.NotNil(t, msg)

			// The AES key is the room private key directly.
			block, err := aes.NewCipher(priv)
			require.NoError(t, err)
			gcm, err := cipher.NewGCM(block)
			require.NoError(t, err)

			plaintext, err := gcm.Open(nil, msg.Nonce, msg.Ciphertext, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(plaintext))
		})
	}
}
