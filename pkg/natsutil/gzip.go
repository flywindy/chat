package natsutil

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"

	"github.com/nats-io/nats.go"
)

// HeaderContentEncoding and HeaderContentType mirror the HTTP header names so operators
// inspecting NATS payloads can apply familiar conventions.
const (
	HeaderContentEncoding = "Content-Encoding"
	HeaderContentType     = "Content-Type"

	ContentEncodingGzip = "gzip"

	// MaxDecodedPayloadSize is the default decompressed-size cap used by
	// DecodePayload. Aligned with the typical operator-tuned NATS max_payload
	// (256 KiB) — tighter than the upstream 1 MiB default. Realistic push events
	// decompress to ≤ ~25 KB given the 20 KiB body cap enforced by
	// message-gatekeeper, so 256 KiB leaves ~10× headroom for legitimate growth
	// while keeping the gzip-bomb amplification ceiling reasonable.
	//
	// Callers who want a different cap (e.g. a service whose operator pinned
	// max_payload to 1 MiB, or one that needs a tighter cap on small events)
	// use DecodePayloadWithLimit(msg, maxBytes).
	MaxDecodedPayloadSize = 256 << 10 // 256 KiB
)

// gzipWriterPool amortises gzip.Writer allocations across publishers; the writer holds
// a ~64 KB internal buffer that would otherwise churn the GC under sustained publish load.
// BestSpeed (level 1) is used deliberately: on the per-message publish hot path the CPU
// cost of the default level 6 dominated profiles, while the ~10-20% larger output stays
// well within the NATS max_payload headroom (push events decompress to ≤ ~25 KB vs a
// 256 KiB cap). Decoders are level-agnostic, so this is transparent to consumers.
var gzipWriterPool = sync.Pool{
	New: func() any {
		// NewWriterLevel only errors on an invalid level; BestSpeed is a valid constant.
		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return w
	},
}

// GzipPayload returns a gzip-compressed copy of payload. Allocates a fresh slice
// so the caller may reuse the input buffer without aliasing the output.
func GzipPayload(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(payload) / 2)
	gz, _ := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(&buf)
	// Reset to io.Discard before returning to the pool so the writer does not
	// retain a reference to the buffer (which may be large on big payloads).
	defer func() {
		gz.Reset(io.Discard)
		gzipWriterPool.Put(gz)
	}()
	if _, err := gz.Write(payload); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// buildMsg builds a *nats.Msg with Content-Type set (defaulting to application/json
// for payload-encoded events) and, when gz is true, Content-Encoding: gzip so a
// consumer using DecodePayload knows whether to decompress.
func buildMsg(subject string, data []byte, contentType string, gz bool) *nats.Msg {
	if contentType == "" {
		contentType = "application/json"
	}
	msg := &nats.Msg{
		Subject: subject,
		Header:  nats.Header{},
		Data:    data,
	}
	msg.Header.Set(HeaderContentType, contentType)
	if gz {
		msg.Header.Set(HeaderContentEncoding, ContentEncodingGzip)
	}
	return msg
}

// NewGzipMsg builds a *nats.Msg with payload gzipped and Content-Encoding/Content-Type
// headers set so a consumer using DecodePayload can transparently decompress.
// contentType may be empty; the helper sets "application/json" by default for payload-encoded events.
func NewGzipMsg(subject string, payload []byte, contentType string) (*nats.Msg, error) {
	encoded, err := GzipPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("build gzip message: %w", err)
	}
	return buildMsg(subject, encoded, contentType, true), nil
}

// NewMaybeGzipMsg gzips payload only when it is at least minGzipBytes; smaller
// payloads are sent verbatim with no Content-Encoding, so DecodePayload passes
// them through. This avoids spending CPU gzipping small payloads that barely
// shrink (gzip framing alone is ~18 bytes). A minGzipBytes <= 0 always
// compresses, matching NewGzipMsg.
func NewMaybeGzipMsg(subject string, payload []byte, contentType string, minGzipBytes int) (*nats.Msg, error) {
	if minGzipBytes > 0 && len(payload) < minGzipBytes {
		return buildMsg(subject, payload, contentType, false), nil
	}
	return NewGzipMsg(subject, payload, contentType)
}

// DecodePayload decodes using the default MaxDecodedPayloadSize cap. For a
// configurable cap (e.g. wired from a service's env var) use DecodePayloadWithLimit.
func DecodePayload(msg *nats.Msg) ([]byte, error) {
	return DecodePayloadWithLimit(msg, MaxDecodedPayloadSize)
}

// DecodePayloadWithLimit returns msg.Data verbatim when uncompressed, or the
// gunzipped bytes when Content-Encoding is "gzip". maxBytes caps the post-gunzip
// size so a gzip bomb can't blow up the consumer; the wire-side NATS max_payload
// is independent (typically 256 KiB - 1 MiB depending on operator config) and
// must be configured at the server level. Unknown encodings produce an error so
// consumers fail loudly rather than silently mis-parsing. A maxBytes of zero or
// negative falls back to MaxDecodedPayloadSize.
func DecodePayloadWithLimit(msg *nats.Msg, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxDecodedPayloadSize
	}
	enc := ""
	if msg.Header != nil {
		enc = msg.Header.Get(HeaderContentEncoding)
	}
	switch enc {
	case "", "identity":
		return msg.Data, nil
	case ContentEncodingGzip:
		r, err := gzip.NewReader(bytes.NewReader(msg.Data))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer r.Close()
		// Read up to maxBytes+1 so we can detect overflow without allocating
		// beyond the cap. Bounds gzip-bomb amplification (~1000× on pathological inputs).
		out, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
		if err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
		if len(out) > maxBytes {
			return nil, fmt.Errorf("gzip payload exceeds %d bytes", maxBytes)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
}
