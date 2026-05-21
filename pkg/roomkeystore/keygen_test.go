package roomkeystore_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/roomkeystore"
)

func TestGenerateKeyPair_Shape(t *testing.T) {
	pair, err := roomkeystore.GenerateKeyPair()
	require.NoError(t, err)
	assert.Len(t, pair.PrivateKey, 32)
}

func TestGenerateKeyPair_Distinct(t *testing.T) {
	a, err := roomkeystore.GenerateKeyPair()
	require.NoError(t, err)
	b, err := roomkeystore.GenerateKeyPair()
	require.NoError(t, err)
	assert.False(t, bytes.Equal(a.PrivateKey, b.PrivateKey))
}
