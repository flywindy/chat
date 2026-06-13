package roomkeystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoStore is the MongoDB-backed implementation of RoomKeyStore. The room
// encryption key lives as an "encKey" sub-document inside the room's document
// in the rooms collection, so a key shares the lifecycle of its room and is
// read in the same database as room metadata.
type mongoStore struct {
	col         *mongo.Collection
	gracePeriod time.Duration
	now         func() time.Time
}

// NewMongoStore returns a RoomKeyStore that stores keys in the encKey field of
// documents in col (the rooms collection). gracePeriod is how long a rotated-out
// previous key remains valid for decrypt. The underlying mongo client is owned
// by the caller; Close is a no-op.
func NewMongoStore(col *mongo.Collection, gracePeriod time.Duration) RoomKeyStore {
	return newMongoStore(col, gracePeriod)
}

func newMongoStore(col *mongo.Collection, gracePeriod time.Duration) *mongoStore {
	return &mongoStore{col: col, gracePeriod: gracePeriod, now: time.Now}
}

var encKeyProjection = bson.M{"encKey": 1}

// InitialKeyDoc returns the encKey sub-document for a freshly provisioned room
// key at version 0. Callers that persist the key inline with the room document
// at creation time (rather than via a follow-up Set) use this so the encKey BSON
// schema stays owned by this package. The previous-key slot is left unset until
// the first Rotate.
func InitialKeyDoc(pair RoomKeyPair) bson.M {
	return bson.M{"priv": pair.PrivateKey, "ver": 0}
}

// setCurrent overwrites the current key slot with priv stamped at version,
// without touching the previous-key slot. Returns ErrRoomNotFound (unwrapped)
// if no room document matched, leaving the caller to add op-specific context.
func (s *mongoStore) setCurrent(ctx context.Context, roomID string, priv []byte, version int) error {
	res, err := s.col.UpdateByID(ctx, roomID, bson.M{"$set": bson.M{
		"encKey.priv": priv,
		"encKey.ver":  version,
	}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrRoomNotFound
	}
	return nil
}

// Set writes pair as the room's current key at version 0 without touching the
// previous-key slot. Returns ErrRoomNotFound if no room document exists.
func (s *mongoStore) Set(ctx context.Context, roomID string, pair RoomKeyPair) (int, error) {
	if err := s.setCurrent(ctx, roomID, pair.PrivateKey, 0); err != nil {
		return 0, fmt.Errorf("set room key: %w", err)
	}
	return 0, nil
}

// SetWithVersion overwrites the current key slot with pair stamped at version,
// without touching the previous-key slot. Returns ErrRoomNotFound if no room
// document exists.
func (s *mongoStore) SetWithVersion(ctx context.Context, roomID string, pair RoomKeyPair, version int) error {
	if err := s.setCurrent(ctx, roomID, pair.PrivateKey, version); err != nil {
		return fmt.Errorf("set room key with version %d: %w", version, err)
	}
	return nil
}

// Get returns the room's current key, or (nil, nil) when the room or its key is absent.
func (s *mongoStore) Get(ctx context.Context, roomID string) (*VersionedKeyPair, error) {
	var doc roomKeyDoc
	err := s.col.FindOne(ctx, bson.M{"_id": roomID}, options.FindOne().SetProjection(encKeyProjection)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get room key: %w", err)
	}
	if doc.EncKey == nil {
		return nil, nil
	}
	vp, err := doc.EncKey.versioned()
	if err != nil {
		return nil, fmt.Errorf("get room key: %w", err)
	}
	return vp, nil
}

// GetMany returns current key pairs for the rooms that have one; rooms without a
// document or without a key are omitted from the result map.
func (s *mongoStore) GetMany(ctx context.Context, roomIDs []string) (map[string]*VersionedKeyPair, error) {
	out := make(map[string]*VersionedKeyPair, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}
	cur, err := s.col.Find(ctx, bson.M{"_id": bson.M{"$in": roomIDs}}, options.Find().SetProjection(encKeyProjection))
	if err != nil {
		return nil, fmt.Errorf("get many room keys: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	for cur.Next(ctx) {
		var doc roomKeyDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("get many room keys: decode: %w", err)
		}
		if doc.EncKey == nil {
			continue
		}
		vp, err := doc.EncKey.versioned()
		if err != nil {
			return nil, fmt.Errorf("get many room keys: room %s: %w", doc.ID, err)
		}
		out[doc.ID] = vp
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("get many room keys: cursor: %w", err)
	}
	return out, nil
}

// GetByVersion returns the key pair matching version from the current slot, or
// from the previous slot while its grace window is still open. Returns (nil, nil)
// when neither matches.
func (s *mongoStore) GetByVersion(ctx context.Context, roomID string, version int) (*RoomKeyPair, error) {
	var doc roomKeyDoc
	err := s.col.FindOne(ctx, bson.M{"_id": roomID}, options.FindOne().SetProjection(encKeyProjection)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get room key by version: %w", err)
	}
	if doc.EncKey == nil {
		return nil, nil
	}
	pair, err := doc.EncKey.pairForVersion(version, s.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("get room key by version: %w", err)
	}
	return pair, nil
}

// Rotate atomically demotes the current key into the previous slot (stamped with
// a grace-period expiry), increments the version, and installs newPair as the
// current key. Returns the new version, or ErrNoCurrentKey if the room has no
// current key. The whole transition runs as one aggregation-pipeline update so
// no concurrent reader observes a partially-rotated key.
func (s *mongoStore) Rotate(ctx context.Context, roomID string, newPair RoomKeyPair) (int, error) {
	expireAt := s.now().UTC().Add(s.gracePeriod)
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.M{
			"encKey.prevPriv":      "$encKey.priv",
			"encKey.prevVer":       "$encKey.ver",
			"encKey.prevExpiresAt": expireAt,
			"encKey.priv":          newPair.PrivateKey,
			"encKey.ver":           bson.M{"$add": bson.A{"$encKey.ver", 1}},
		}}},
	}
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetProjection(bson.M{"encKey.ver": 1})
	var updated roomKeyDoc
	err := s.col.FindOneAndUpdate(ctx,
		bson.M{"_id": roomID, "encKey.priv": bson.M{"$exists": true}},
		pipeline, opts,
	).Decode(&updated)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, ErrNoCurrentKey
	}
	if err != nil {
		return 0, fmt.Errorf("rotate room key: %w", err)
	}
	if updated.EncKey == nil {
		return 0, fmt.Errorf("rotate room key: missing key after update")
	}
	return updated.EncKey.Ver, nil
}

// Delete removes the entire encKey sub-document. No-op when the room or key is absent.
func (s *mongoStore) Delete(ctx context.Context, roomID string) error {
	if _, err := s.col.UpdateByID(ctx, roomID, bson.M{"$unset": bson.M{"encKey": ""}}); err != nil {
		return fmt.Errorf("delete room key: %w", err)
	}
	return nil
}

// Close is a no-op: the mongo client is owned and closed by the caller.
func (s *mongoStore) Close() error { return nil }

// keyDoc is the BSON shape of the encryption-key sub-document embedded in a
// room document under the "encKey" field. The current key (priv/ver) is always
// present once provisioned; the previous-key slot (prevPriv/prevVer/
// prevExpiresAt) is populated by Rotate and ignored once prevExpiresAt elapses.
type keyDoc struct {
	Priv          []byte     `bson:"priv"`
	Ver           int        `bson:"ver"`
	PrevPriv      []byte     `bson:"prevPriv,omitempty"`
	PrevVer       int        `bson:"prevVer,omitempty"`
	PrevExpiresAt *time.Time `bson:"prevExpiresAt,omitempty"`
}

// roomKeyDoc projects just the _id and encKey fields of a room document.
type roomKeyDoc struct {
	ID     string  `bson:"_id"`
	EncKey *keyDoc `bson:"encKey"`
}

// versioned converts the current key slot into a VersionedKeyPair, validating
// the secret length.
func (d *keyDoc) versioned() (*VersionedKeyPair, error) {
	if err := validateSecret(d.Priv); err != nil {
		return nil, fmt.Errorf("decode current key: %w", err)
	}
	return &VersionedKeyPair{Version: d.Ver, KeyPair: RoomKeyPair{PrivateKey: d.Priv}}, nil
}

// pairForVersion returns the key pair matching version from either the current
// slot or the previous slot (only while the previous slot's grace window is
// still open at now). Returns (nil, nil) when neither slot matches.
func (d *keyDoc) pairForVersion(version int, now time.Time) (*RoomKeyPair, error) {
	// Match the Valkey store: a slot whose version matches but whose secret is
	// corrupt surfaces an error rather than masquerading as a miss. The previous
	// slot only counts while its grace window is open; the grace check also
	// guards against a never-rotated room's zero-value PrevVer matching version 0.
	if d.Ver == version {
		if err := validateSecret(d.Priv); err != nil {
			return nil, fmt.Errorf("decode current key: %w", err)
		}
		return &RoomKeyPair{PrivateKey: d.Priv}, nil
	}
	if d.PrevExpiresAt != nil && now.Before(*d.PrevExpiresAt) && d.PrevVer == version {
		if err := validateSecret(d.PrevPriv); err != nil {
			return nil, fmt.Errorf("decode previous key: %w", err)
		}
		return &RoomKeyPair{PrivateKey: d.PrevPriv}, nil
	}
	return nil, nil
}

// validateSecret ensures a stored secret is the expected 32-byte length.
func validateSecret(priv []byte) error {
	if len(priv) != 32 {
		return fmt.Errorf("invalid key length %d", len(priv))
	}
	return nil
}
