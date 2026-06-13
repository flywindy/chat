package roomkeystore

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitialKeyDoc(t *testing.T) {
	priv := bytes.Repeat([]byte{0x07}, 32)
	doc := InitialKeyDoc(RoomKeyPair{PrivateKey: priv})

	assert.Equal(t, priv, doc["priv"], "priv must carry the raw secret bytes")
	assert.Equal(t, 0, doc["ver"], "a freshly provisioned key is version 0")
	_, hasPrev := doc["prevPriv"]
	assert.False(t, hasPrev, "previous-key slot must be unset until the first Rotate")
}

func TestKeyDoc_versioned(t *testing.T) {
	priv := bytes.Repeat([]byte{0xAB}, 32)

	t.Run("valid current key", func(t *testing.T) {
		d := &keyDoc{Priv: priv, Ver: 3}
		got, err := d.versioned()
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, 3, got.Version)
		assert.Equal(t, priv, got.KeyPair.PrivateKey)
	})

	t.Run("invalid length errors", func(t *testing.T) {
		d := &keyDoc{Priv: []byte{0x01, 0x02}, Ver: 0}
		_, err := d.versioned()
		require.Error(t, err)
	})
}

func TestKeyDoc_pairForVersion(t *testing.T) {
	cur := bytes.Repeat([]byte{0x11}, 32)
	prev := bytes.Repeat([]byte{0x22}, 32)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	tests := []struct {
		name    string
		doc     *keyDoc
		version int
		want    []byte // nil means expect (nil, nil)
		wantErr bool   // expect a non-nil error (corrupt slot)
	}{
		{
			name:    "matches current",
			doc:     &keyDoc{Priv: cur, Ver: 5},
			version: 5,
			want:    cur,
		},
		{
			name:    "matches previous within grace",
			doc:     &keyDoc{Priv: cur, Ver: 5, PrevPriv: prev, PrevVer: 4, PrevExpiresAt: &future},
			version: 4,
			want:    prev,
		},
		{
			name:    "previous expired returns nil",
			doc:     &keyDoc{Priv: cur, Ver: 5, PrevPriv: prev, PrevVer: 4, PrevExpiresAt: &past},
			version: 4,
			want:    nil,
		},
		{
			name:    "unknown version returns nil",
			doc:     &keyDoc{Priv: cur, Ver: 5, PrevPriv: prev, PrevVer: 4, PrevExpiresAt: &future},
			version: 99,
			want:    nil,
		},
		{
			name:    "no previous slot, version not current",
			doc:     &keyDoc{Priv: cur, Ver: 5},
			version: 4,
			want:    nil,
		},
		{
			name:    "previous with nil expiry treated as absent",
			doc:     &keyDoc{Priv: cur, Ver: 5, PrevPriv: prev, PrevVer: 4},
			version: 4,
			want:    nil,
		},
		{
			// Matches the Valkey store: a current slot whose version matches but
			// whose secret is corrupt is an error, not a silent miss.
			name:    "current version matches but secret corrupt errors",
			doc:     &keyDoc{Priv: []byte{0x01, 0x02}, Ver: 5},
			version: 5,
			wantErr: true,
		},
		{
			name:    "previous within grace matches but secret corrupt errors",
			doc:     &keyDoc{Priv: cur, Ver: 5, PrevPriv: []byte{0x01}, PrevVer: 4, PrevExpiresAt: &future},
			version: 4,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.doc.pairForVersion(tc.version, now)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.PrivateKey)
		})
	}
}
