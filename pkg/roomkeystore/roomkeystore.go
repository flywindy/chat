package roomkeystore

import (
	"context"
	"errors"
)

// ErrNoCurrentKey is returned by Rotate when no current key exists for the room.
var ErrNoCurrentKey = errors.New("no current key")

// ErrRoomNotFound is returned by Set/SetWithVersion when no room document
// exists to attach the key to. A room key is a field of its room document, so
// the room must already exist before a key can be written.
var ErrRoomNotFound = errors.New("room not found")

// RoomKeyPair holds the 32-byte room secret used directly as the AES-256-GCM key by roomcrypto.
type RoomKeyPair struct {
	PrivateKey []byte // 32-byte secret; used directly as AES-256-GCM key material
}

// VersionedKeyPair pairs a key pair with its store-assigned version number.
type VersionedKeyPair struct {
	Version int
	KeyPair RoomKeyPair
}

// RoomKeyStore defines storage operations for room encryption secrets.
type RoomKeyStore interface {
	Set(ctx context.Context, roomID string, pair RoomKeyPair) (int, error)
	// SetWithVersion overwrites the current key for roomID with pair stamped at the
	// caller-supplied version. Intended for the rotate fallback path where a room
	// has no current key yet but a specific version must be adopted so on-wire
	// message envelopes match the version clients hold. Does not touch the
	// previous-key slot.
	SetWithVersion(ctx context.Context, roomID string, pair RoomKeyPair, version int) error
	Get(ctx context.Context, roomID string) (*VersionedKeyPair, error)
	GetMany(ctx context.Context, roomIDs []string) (map[string]*VersionedKeyPair, error)
	GetByVersion(ctx context.Context, roomID string, version int) (*RoomKeyPair, error)
	Rotate(ctx context.Context, roomID string, newPair RoomKeyPair) (int, error)
	Delete(ctx context.Context, roomID string) error
	Close() error
}
