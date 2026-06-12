package cassrepo

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/gocql/gocql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/history-service/internal/models"
)

// BenchmarkBuildScanValues mirrors the per-row hot path: mapping the full
// messages_by_room column set onto a fresh Message for every scanned row.
func BenchmarkBuildScanValues(b *testing.B) {
	cols := strings.Split(strings.ReplaceAll(baseColumns, " ", ""), ",")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var m models.Message
		if _, _, ok := buildScanValues(&m, cols); !ok {
			b.Fatal("unexpected column miss")
		}
	}
}

func TestNewCursor_Empty(t *testing.T) {
	c, err := NewCursor("")
	require.NoError(t, err)
	assert.Empty(t, c.Raw())
	assert.Equal(t, "", c.Encode())
}

func TestNewCursor_Valid(t *testing.T) {
	state := []byte{0x01, 0x02, 0x03}
	encoded := base64.StdEncoding.EncodeToString(state)

	c, err := NewCursor(encoded)
	require.NoError(t, err)
	assert.Equal(t, state, c.Raw())
	assert.Equal(t, encoded, c.Encode())
}

func TestNewCursor_Invalid(t *testing.T) {
	_, err := NewCursor("not-valid-base64!!!")
	require.Error(t, err)
}

func TestCursor_RoundTrip(t *testing.T) {
	state := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	original := &Cursor{state: state}

	encoded := original.Encode()
	decoded, err := NewCursor(encoded)
	require.NoError(t, err)
	assert.Equal(t, state, decoded.Raw())
}

func TestParsePageRequest_Defaults(t *testing.T) {
	q, err := ParsePageRequest("", 0)
	require.NoError(t, err)
	assert.Empty(t, q.Cursor.Raw())
	assert.Equal(t, 50, q.PageSize)
}

func TestParsePageRequest_WithValues(t *testing.T) {
	state := []byte{0x01}
	encoded := base64.StdEncoding.EncodeToString(state)

	q, err := ParsePageRequest(encoded, 25)
	require.NoError(t, err)
	assert.Equal(t, state, q.Cursor.Raw())
	assert.Equal(t, 25, q.PageSize)
}

func TestParsePageRequest_InvalidCursor(t *testing.T) {
	_, err := ParsePageRequest("bad!!!", 10)
	require.Error(t, err)
}

func TestParsePageRequest_ClampsPageSize(t *testing.T) {
	q, err := ParsePageRequest("", 999)
	require.NoError(t, err)
	assert.Equal(t, 100, q.PageSize)
}

func TestQueryBuilder_Chaining(t *testing.T) {
	cursor := &Cursor{state: []byte{0xAB}}
	b := NewQueryBuilder(nil).
		WithCursor(cursor).
		WithPageSize(25)

	assert.Equal(t, cursor, b.cursor)
	assert.Equal(t, 25, b.pageSize)
}

func TestQueryBuilder_Fetch_NilQuery(t *testing.T) {
	b := NewQueryBuilder(nil)
	_, err := b.Fetch(func(iter *gocql.Iter) {
		t.Fatal("scan should not be called for nil query")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil query")
}

func TestNewCursor_Invalid_WrapsError(t *testing.T) {
	_, err := NewCursor("not-valid-base64!!!")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode cursor")
}

func TestParsePageRequest_InvalidCursor_WrapsError(t *testing.T) {
	_, err := ParsePageRequest("bad!!!", 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse page request cursor")
}

func TestStructScan_NonPointer(t *testing.T) {
	type S struct {
		Name string `cql:"name"`
	}
	ok, err := structScan(nil, S{Name: "x"})
	assert.False(t, ok)
	assert.NoError(t, err)
}

func TestStructScan_PointerToNonStruct(t *testing.T) {
	s := "hello"
	ok, err := structScan(nil, &s)
	assert.False(t, ok)
	assert.NoError(t, err)
}

// TestBuildScanValues exercises the column-matching helper including the unmapped-column hard-error path.
func TestBuildScanValues(t *testing.T) {
	type Row struct {
		RoomID    string `cql:"room_id"`
		CreatedAt int64  `cql:"created_at"`
		Msg       string `cql:"msg"`
	}

	tests := []struct {
		name        string
		colNames    []string
		wantOK      bool
		wantMissing string
	}{
		{
			name:     "all columns map",
			colNames: []string{"room_id", "created_at", "msg"},
			wantOK:   true,
		},
		{
			name:        "unmapped column at front",
			colNames:    []string{"unknown_col", "room_id"},
			wantOK:      false,
			wantMissing: "unknown_col",
		},
		{
			name:        "unmapped column in middle",
			colNames:    []string{"room_id", "ghost", "msg"},
			wantOK:      false,
			wantMissing: "ghost",
		},
		{
			name:        "unmapped column at end",
			colNames:    []string{"room_id", "created_at", "msg", "extra"},
			wantOK:      false,
			wantMissing: "extra",
		},
		{
			name:     "empty columns",
			colNames: []string{},
			wantOK:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var row Row
			vals, missingCol, ok := buildScanValues(&row, tc.colNames)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantMissing, missingCol)
			if tc.wantOK {
				assert.Len(t, vals, len(tc.colNames))
			}
		})
	}
}

func TestBuildScanValues_NonPointer(t *testing.T) {
	type Row struct {
		ID string `cql:"id"`
	}
	vals, missingCol, ok := buildScanValues(Row{}, []string{"id"})
	assert.False(t, ok)
	assert.Empty(t, missingCol)
	assert.Nil(t, vals)
}

type scanRowA struct {
	RoomID string `cql:"room_id"`
	Msg    string `cql:"msg"`
}

type scanRowB struct {
	ThreadID string `cql:"thread_id"`
	Count    int64  `cql:"count"`
}

// TestBuildScanValues_WritesToCorrectFields verifies the returned pointers
// address the right struct fields, and that interleaving distinct types does
// not let a per-type field-index cache cross-contaminate.
func TestBuildScanValues_WritesToCorrectFields(t *testing.T) {
	var a scanRowA
	valsA, _, okA := buildScanValues(&a, []string{"msg", "room_id"})
	require.True(t, okA)
	require.Len(t, valsA, 2)

	var b scanRowB
	valsB, _, okB := buildScanValues(&b, []string{"count", "thread_id"})
	require.True(t, okB)
	require.Len(t, valsB, 2)

	// Write through A's pointers after B was processed — a type-keyed cache
	// must still resolve A's columns to A's fields.
	*(valsA[0].(*string)) = "hello"  // msg
	*(valsA[1].(*string)) = "room-1" // room_id
	*(valsB[0].(*int64)) = 7         // count
	*(valsB[1].(*string)) = "thr-1"  // thread_id

	assert.Equal(t, scanRowA{RoomID: "room-1", Msg: "hello"}, a)
	assert.Equal(t, scanRowB{ThreadID: "thr-1", Count: 7}, b)
}

// TestBuildScanValues_ConcurrentDistinctTypes guards the shared field-index
// cache against data races; run with -race.
func TestBuildScanValues_ConcurrentDistinctTypes(t *testing.T) {
	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				var a scanRowA
				_, _, ok := buildScanValues(&a, []string{"room_id", "msg"})
				assert.True(t, ok)
			} else {
				var b scanRowB
				_, _, ok := buildScanValues(&b, []string{"thread_id", "count"})
				assert.True(t, ok)
			}
		}(i)
	}
	wg.Wait()
}

func TestNewCursor_TooLong(t *testing.T) {
	// 515 raw bytes encode to 688 base64 chars, which exceeds EncodedLen(512)=684.
	huge := base64.StdEncoding.EncodeToString(make([]byte, maxCursorBytes+3))
	_, err := NewCursor(huge)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode cursor")
}
