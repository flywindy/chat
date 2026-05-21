package roomcrypto

import (
	"crypto/rand"
	"io"
	"testing"
)

// BenchmarkEncoder_Encode measures the hot path: encoder cache hit +
// nonce read + AES-GCM seal. This is what broadcast-worker pays per
// message at steady state.
func BenchmarkEncoder_Encode(b *testing.B) {
	priv := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, priv); err != nil {
		b.Fatal(err)
	}
	enc := NewEncoder()

	// Warm the cache so we measure steady-state, not one-time AES cipher construction.
	if _, err := enc.Encode("room-1", "warm", priv, 1); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode("room-1", "hello, world — a typical short chat message", priv, 1); err != nil {
			b.Fatal(err)
		}
	}
}
