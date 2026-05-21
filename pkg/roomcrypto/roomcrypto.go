package roomcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
)

// EncryptedMessage holds the output of Encode.
// []byte fields marshal to base64 in JSON automatically.
//
// Note: this struct intentionally deviates from the project convention of including bson tags.
// It is a serialisation-only type sent to clients over JSON; it is never written to MongoDB.
type EncryptedMessage struct {
	// key version used to encrypt; matches roomkeystore VersionedKeyPair.Version
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`      // 12 bytes, AES-GCM nonce
	Ciphertext []byte `json:"ciphertext"` // encrypted content + 16-byte AES-GCM tag
}

// Encoder holds the per-(roomId, version) AES-GCM cipher cache. The room
// secret is used directly as an AES-256 key. Construct one per process and
// share it across goroutines.
type Encoder struct {
	mu    sync.RWMutex
	cache map[encoderCacheKey]cipher.AEAD
	rand  io.Reader
	max   int
}

type encoderCacheKey struct {
	roomID  string
	version int
}

// EncoderOption configures an Encoder at construction time.
type EncoderOption func(*Encoder)

// WithMaxCacheEntries sets the upper bound on the per-(roomId, version)
// AES-GCM cache. When exceeded, the entry with the lowest version is
// evicted. Default 4096.
func WithMaxCacheEntries(n int) EncoderOption {
	return func(e *Encoder) { e.max = n }
}

// WithRand overrides the source of randomness used for nonce generation.
// Intended for testing only.
func WithRand(r io.Reader) EncoderOption {
	return func(e *Encoder) { e.rand = r }
}

// NewEncoder constructs an Encoder with default cache size 4096 and
// crypto/rand.Reader as the randomness source.
func NewEncoder(opts ...EncoderOption) *Encoder {
	e := &Encoder{
		cache: make(map[encoderCacheKey]cipher.AEAD),
		rand:  rand.Reader,
		max:   4096,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Encode encrypts content using the AES-256-GCM cipher keyed directly from
// roomPrivateKey for the given (roomID, version). The cipher is cached on the
// Encoder; repeat calls for the same (roomID, version) reuse the cached entry.
func (e *Encoder) Encode(roomID, content string, roomPrivateKey []byte, version int) (*EncryptedMessage, error) {
	gcm, err := e.aeadFor(roomID, roomPrivateKey, version)
	if err != nil {
		return nil, fmt.Errorf("preparing cipher for room %s v%d: %w", roomID, version, err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(e.rand, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(content), nil)
	return &EncryptedMessage{
		Version:    version,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	}, nil
}

func (e *Encoder) aeadFor(roomID string, roomPrivateKey []byte, version int) (cipher.AEAD, error) {
	if len(roomPrivateKey) != 32 {
		return nil, fmt.Errorf("room private key must be 32 bytes, got %d", len(roomPrivateKey))
	}

	key := encoderCacheKey{roomID: roomID, version: version}

	e.mu.RLock()
	gcm, ok := e.cache[key]
	e.mu.RUnlock()
	if ok {
		return gcm, nil
	}

	block, err := aes.NewCipher(roomPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	newGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM wrapper: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	// Double-check under write lock — another goroutine may have populated.
	if existing, ok := e.cache[key]; ok {
		return existing, nil
	}
	if len(e.cache) >= e.max {
		e.evictLowestVersionLocked()
	}
	e.cache[key] = newGCM
	return newGCM, nil
}

// evictLowestVersionLocked drops the entry with the lowest version
// across all rooms. This policy is deliberately simple (not LRU): hot
// rooms keep their entries via frequent insertions; rare rooms with
// old, unused versions are dropped first. Caller must hold e.mu for writing.
//
// Precondition: len(e.cache) > 0 (only called when cache is full).
func (e *Encoder) evictLowestVersionLocked() {
	var (
		victim    encoderCacheKey
		haveFirst bool
	)
	for k := range e.cache {
		if !haveFirst || k.version < victim.version {
			victim = k
			haveFirst = true
		}
	}
	if haveFirst {
		delete(e.cache, victim)
	}
}

// cacheLen is exported only for tests in the same package.
func (e *Encoder) cacheLen() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.cache)
}
