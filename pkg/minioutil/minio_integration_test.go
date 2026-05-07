//go:build integration

package minioutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/hmchangw/chat/pkg/testutil"
)

// TestIntegration_Connect_Smoke verifies the shared testutil.MinIO wiring
// works end-to-end. Connect itself can't run against the testcontainers
// MinIO via testutil.MinIO (which constructs its own client), so this
// test exercises only the shared client + bucket. Subsequent integration
// tests in this file (added by Tasks 11-15) layer Bucket[T] coverage on
// top of the same testutil.MinIO helper.
func TestIntegration_Connect_Smoke(t *testing.T) {
	client, bucket := testutil.MinIO(t, "minioutil")
	require.NotNil(t, client)
	require.NotEmpty(t, bucket)

	// Verify the bucket exists from the client's perspective.
	exists, err := client.BucketExists(context.Background(), bucket)
	require.NoError(t, err)
	require.True(t, exists)
}

// TestIntegration_NewBucket_Success verifies the constructor accepts a bucket
// that exists on the server.
func TestIntegration_NewBucket_Success(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")

	type doc struct{ Name string `json:"name"` }
	b, err := NewBucket[doc](context.Background(), client, bucketName)
	require.NoError(t, err)
	require.NotNil(t, b)
}

// TestIntegration_NewBucket_MissingBucket verifies the constructor fails fast
// when the named bucket does not exist on the server (the misconfigured
// MINIO_BUCKET case the spec calls out).
func TestIntegration_NewBucket_MissingBucket(t *testing.T) {
	client, _ := testutil.MinIO(t, "minioutil")

	type doc struct{ Name string `json:"name"` }
	_, err := NewBucket[doc](context.Background(), client, "definitely-does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "definitely-does-not-exist")
}

// TestIntegration_Put_RoundTrip verifies Put writes a JSON-encoded object
// with the documented content-type. Reads the raw object back via the
// underlying client (NOT via Bucket.Get, which is tested separately) so
// this test isolates Put's contract.
func TestIntegration_Put_RoundTrip(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k1", doc{Name: "alpha", Count: 7}))

	// Read raw via underlying client — verifies bytes-on-the-wire and content-type.
	obj, err := client.GetObject(ctx, bucketName, "k1", minio.GetObjectOptions{})
	require.NoError(t, err)
	defer obj.Close()
	info, err := obj.Stat()
	require.NoError(t, err)
	assert.Equal(t, "application/json; charset=utf-8", info.ContentType)

	body, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"alpha","count":7}`, string(body))
}

// TestIntegration_Put_Overwrites verifies repeat Put on the same key
// replaces the object (S3 native PUT semantics — no versioning configured).
func TestIntegration_Put_Overwrites(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k", doc{V: 1}))
	require.NoError(t, b.Put(ctx, "k", doc{V: 2}))

	obj, err := client.GetObject(ctx, bucketName, "k", minio.GetObjectOptions{})
	require.NoError(t, err)
	defer obj.Close()
	body, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":2}`, string(body))
}

// TestIntegration_Get_RoundTrip verifies a Put followed by a Get returns
// the originally-stored value. Uses Put to seed (already covered by Task 12
// tests, so trusted here).
func TestIntegration_Get_RoundTrip(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	want := doc{Name: "alpha", Count: 7}
	require.NoError(t, b.Put(ctx, "k1", want))

	got, err := b.Get(ctx, "k1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

// TestIntegration_Get_MissingKey verifies the (nil, nil) contract for a
// key that does not exist — matches Collection.FindOne semantics so callers
// can branch on result == nil instead of catching errors.
func TestIntegration_Get_MissingKey(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	got, err := b.Get(ctx, "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestIntegration_Get_MalformedJSON verifies decode errors propagate as
// errors (NOT as silent nil) when the stored object is not valid JSON for
// type T. Seeds the bucket with raw garbage via the underlying client to
// bypass Put's JSON encoding.
func TestIntegration_Get_MalformedJSON(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	garbage := []byte("not json at all")
	_, err = client.PutObject(ctx, bucketName, "bad", bytes.NewReader(garbage), int64(len(garbage)), minio.PutObjectOptions{})
	require.NoError(t, err)

	_, err = b.Get(ctx, "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode", "decode-error path must surface as a 'decode' wrap, not a transport/auth error")
}

// TestIntegration_List_Prefix verifies prefix filtering returns only matching
// keys, in S3's lexicographic order.
func TestIntegration_List_Prefix(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "users/alice", doc{V: 1}))
	require.NoError(t, b.Put(ctx, "users/bob", doc{V: 2}))
	require.NoError(t, b.Put(ctx, "rooms/main", doc{V: 3}))

	keys, err := b.List(ctx, "users/", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"users/alice", "users/bob"}, keys)
}

// TestIntegration_List_ZeroMaxKeysReturnsAll verifies that maxKeys=0
// goes through the defaultListCap fallback branch and returns every
// matching key when the prefix has fewer than defaultListCap (1000)
// objects. Seeding 1001 keys to exercise the cap engaging would add
// ~30s of Put traffic to CI for marginal incremental coverage; the
// cap-engages-and-breaks-cleanly behavior is exercised at small N
// by TestIntegration_List_MaxKeysCap (which also goleak-verifies the
// early-break cleanup). This test only proves "maxKeys=0 doesn't
// crash and doesn't truncate when N < cap".
func TestIntegration_List_ZeroMaxKeysReturnsAll(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.NoError(t, b.Put(ctx, fmt.Sprintf("k-%02d", i), doc{V: i}))
	}

	keys, err := b.List(ctx, "k-", 0)
	require.NoError(t, err)
	assert.Len(t, keys, 5)
}

// TestIntegration_List_MaxKeysCap verifies maxKeys caps the result AND
// that the early break does not leak the underlying minio-go listing
// goroutine. The -race flag does NOT detect goroutine leaks, so goleak
// is the only real guarantee that `defer cancel()` is doing its job.
func TestIntegration_List_MaxKeysCap(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		require.NoError(t, b.Put(ctx, fmt.Sprintf("k-%02d", i), doc{V: i}))
	}

	// Snapshot all goroutines existing BEFORE List runs (including minio-go
	// HTTP keepalive readLoop/writeLoop spawned by the prior Puts). goleak's
	// default ignore list does NOT cover net/http.persistConn goroutines,
	// and minio-go's transport keeps them alive for 60s after their last
	// use -- far longer than goleak's 2s drain budget. IgnoreCurrent()
	// captures the baseline so we only flag NEW goroutines spawned by List.
	preList := goleak.IgnoreCurrent()

	keys, err := b.List(ctx, "k-", 3)
	require.NoError(t, err)
	require.Len(t, keys, 3)
	assert.Equal(t, []string{"k-00", "k-01", "k-02"}, keys)

	// Verify List's early-break + defer cancel() cleaned up minio-go's
	// listing goroutine. Without defer cancel(), the goroutine would be
	// parked on a buffered-channel send and visible here.
	goleak.VerifyNone(t, preList)
}

// TestIntegration_List_EmptyResult verifies an empty (non-nil) slice is
// returned when nothing matches the prefix.
func TestIntegration_List_EmptyResult(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	keys, err := b.List(ctx, "no-such-prefix/", 0)
	require.NoError(t, err)
	require.NotNil(t, keys)
	assert.Empty(t, keys)
}

// TestIntegration_Delete_RemovesExisting verifies Delete removes a
// previously-Put object so a subsequent Get returns (nil, nil).
func TestIntegration_Delete_RemovesExisting(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "k", doc{V: 1}))
	require.NoError(t, b.Delete(ctx, "k"))

	got, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestIntegration_Delete_Idempotent verifies Delete on a missing key
// returns nil (S3 / MinIO native semantics: DELETE returns 204 regardless).
// Callers don't need to pre-check existence or swallow not-found errors.
func TestIntegration_Delete_Idempotent(t *testing.T) {
	client, bucketName := testutil.MinIO(t, "minioutil")
	ctx := context.Background()

	type doc struct{ V int `json:"v"` }
	b, err := NewBucket[doc](ctx, client, bucketName)
	require.NoError(t, err)

	require.NoError(t, b.Delete(ctx, "never-existed"))
	require.NoError(t, b.Delete(ctx, "never-existed")) // and again
}
