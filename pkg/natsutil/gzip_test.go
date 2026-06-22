package natsutil

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGzipPayload_RoundTrip(t *testing.T) {
	in := []byte(strings.Repeat(`{"k":"v"}`, 100))
	encoded, err := GzipPayload(in)
	require.NoError(t, err)
	assert.NotEqual(t, in, encoded, "encoded payload differs from input")
	assert.Less(t, len(encoded), len(in), "highly repetitive JSON should shrink under gzip")

	r, err := gzip.NewReader(bytes.NewReader(encoded))
	require.NoError(t, err)
	defer r.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	assert.Equal(t, in, buf.Bytes())
}

func TestGzipPayload_UsesBestSpeed(t *testing.T) {
	// A push-batch-shaped payload: repetitive enough that level 1 and level 6
	// produce different bytes, so byte-for-byte equality with a BestSpeed
	// reference encoder fails if GzipPayload uses any other level.
	in := []byte(strings.Repeat(`{"account":"alice","muted":false},`, 200))

	var ref bytes.Buffer
	gz, err := gzip.NewWriterLevel(&ref, gzip.BestSpeed)
	require.NoError(t, err)
	_, err = gz.Write(in)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	encoded, err := GzipPayload(in)
	require.NoError(t, err)

	// XFL byte (gzip header offset 8) is 4 when the fastest algorithm was used.
	require.GreaterOrEqual(t, len(encoded), 10, "gzip output has a 10-byte header")
	assert.Equal(t, byte(4), encoded[8], "XFL byte must signal BestSpeed")
	assert.Equal(t, ref.Bytes(), encoded, "output must match a BestSpeed encoder byte-for-byte")
}

func TestGzipPayload_EmptyInput(t *testing.T) {
	encoded, err := GzipPayload(nil)
	require.NoError(t, err)
	// gzip framing means even empty input produces a non-empty (header+trailer) output.
	assert.NotEmpty(t, encoded)
	r, err := gzip.NewReader(bytes.NewReader(encoded))
	require.NoError(t, err)
	defer r.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	require.NoError(t, err)
	assert.Empty(t, buf.Bytes())
}

func TestNewGzipMsg_SetsHeadersAndCompresses(t *testing.T) {
	in := []byte(`{"hello":"world"}`)
	msg, err := NewGzipMsg("foo.bar", in, "")
	require.NoError(t, err)

	assert.Equal(t, "foo.bar", msg.Subject)
	assert.Equal(t, ContentEncodingGzip, msg.Header.Get(HeaderContentEncoding))
	assert.Equal(t, "application/json", msg.Header.Get(HeaderContentType), "default content type")

	decoded, err := DecodePayload(msg)
	require.NoError(t, err)
	assert.Equal(t, in, decoded)
}

func TestNewMaybeGzipMsg(t *testing.T) {
	tests := []struct {
		name         string
		payload      []byte
		minGzipBytes int
		wantGzip     bool
	}{
		{"below threshold sent verbatim", []byte(`{"hello":"world"}`), 1024, false},
		{"at threshold compresses", []byte(strings.Repeat("x", 1024)), 1024, true},
		{"zero threshold always compresses", []byte("tiny"), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := NewMaybeGzipMsg("foo.bar", tt.payload, "", tt.minGzipBytes)
			require.NoError(t, err)
			assert.Equal(t, "application/json", msg.Header.Get(HeaderContentType), "default content type set either way")

			if tt.wantGzip {
				assert.Equal(t, ContentEncodingGzip, msg.Header.Get(HeaderContentEncoding))
				assert.NotEqual(t, tt.payload, msg.Data, "gzipped data differs from input")
			} else {
				assert.Empty(t, msg.Header.Get(HeaderContentEncoding), "small payload must not be gzip-encoded")
				assert.Equal(t, tt.payload, msg.Data, "payload sent verbatim when below threshold")
			}

			decoded, err := DecodePayload(msg)
			require.NoError(t, err)
			assert.Equal(t, tt.payload, decoded, "payload round-trips via the shared decoder")
		})
	}
}

func TestNewGzipMsg_CustomContentType(t *testing.T) {
	msg, err := NewGzipMsg("foo.bar", []byte("hi"), "text/plain")
	require.NoError(t, err)
	assert.Equal(t, "text/plain", msg.Header.Get(HeaderContentType))
}

func TestDecodePayload_NoEncoding_PassesThrough(t *testing.T) {
	msg := &nats.Msg{Data: []byte("raw bytes"), Header: nats.Header{}}
	out, err := DecodePayload(msg)
	require.NoError(t, err)
	assert.Equal(t, []byte("raw bytes"), out)
}

func TestDecodePayload_IdentityEncoding_PassesThrough(t *testing.T) {
	msg := &nats.Msg{Data: []byte("raw"), Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, "identity")
	out, err := DecodePayload(msg)
	require.NoError(t, err)
	assert.Equal(t, []byte("raw"), out)
}

func TestDecodePayload_NilHeader(t *testing.T) {
	msg := &nats.Msg{Data: []byte("raw"), Header: nil}
	out, err := DecodePayload(msg)
	require.NoError(t, err)
	assert.Equal(t, []byte("raw"), out)
}

func TestDecodePayload_UnsupportedEncoding(t *testing.T) {
	msg := &nats.Msg{Data: []byte("x"), Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, "br")
	_, err := DecodePayload(msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported Content-Encoding")
}

func TestDecodePayload_GzipCorrupt(t *testing.T) {
	msg := &nats.Msg{Data: []byte("not gzip"), Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, ContentEncodingGzip)
	_, err := DecodePayload(msg)
	require.Error(t, err)
}

func TestDecodePayload_GzipExceedsMaxDecodedSize(t *testing.T) {
	// Build a payload that decompresses to MaxDecodedPayloadSize+1 bytes. gzip on a
	// constant byte runs at ~1000× compression so the wire bytes stay tiny — exactly
	// the gzip-bomb shape we want the cap to reject.
	oversized := bytes.Repeat([]byte{'a'}, MaxDecodedPayloadSize+1)
	encoded, err := GzipPayload(oversized)
	require.NoError(t, err)
	assert.Less(t, len(encoded), MaxDecodedPayloadSize/100,
		"sanity: highly repetitive input should compress small enough to fit in a single NATS msg")

	msg := &nats.Msg{Data: encoded, Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, ContentEncodingGzip)
	_, err = DecodePayload(msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestDecodePayload_GzipAtMaxDecodedSize(t *testing.T) {
	// Boundary check: a payload that exactly hits the cap must still succeed.
	atLimit := bytes.Repeat([]byte{'b'}, MaxDecodedPayloadSize)
	encoded, err := GzipPayload(atLimit)
	require.NoError(t, err)

	msg := &nats.Msg{Data: encoded, Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, ContentEncodingGzip)
	out, err := DecodePayload(msg)
	require.NoError(t, err)
	assert.Len(t, out, MaxDecodedPayloadSize)
}

func TestDecodePayload_GzipTruncated(t *testing.T) {
	full, err := GzipPayload([]byte("hello world"))
	require.NoError(t, err)
	msg := &nats.Msg{Data: full[:len(full)-3], Header: nats.Header{}}
	msg.Header.Set(HeaderContentEncoding, ContentEncodingGzip)
	_, err = DecodePayload(msg)
	require.Error(t, err)
}
