package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEngine implements the local esEngine interface (Search + GetDoc).
type stubEngine struct {
	searchBody    json.RawMessage
	searchIndices []string
	searchReqBody json.RawMessage
	searchErr     error

	docBody    json.RawMessage
	docFound   bool
	docErr     error
	docIndex   string
	docDocID   string
	docCallCnt int
}

func (s *stubEngine) Search(_ context.Context, indices []string, body json.RawMessage) (json.RawMessage, error) {
	s.searchIndices = indices
	// Copy the body so a caller that reuses its buffer can't mutate the
	// captured assertion value.
	s.searchReqBody = append(json.RawMessage(nil), body...)
	return s.searchBody, s.searchErr
}

func (s *stubEngine) GetDoc(_ context.Context, index, id string) (json.RawMessage, bool, error) {
	s.docIndex, s.docDocID = index, id
	s.docCallCnt++
	return s.docBody, s.docFound, s.docErr
}

func TestESStore_Search_DelegatesToEngine(t *testing.T) {
	eng := &stubEngine{searchBody: json.RawMessage(`{"ok":true}`)}
	s := newESStore(eng, "user-room")

	body := json.RawMessage(`{"query":{"match_all":{}}}`)
	raw, err := s.Search(context.Background(), []string{"messages-*"}, body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(raw))
	assert.Equal(t, []string{"messages-*"}, eng.searchIndices)
	assert.JSONEq(t, string(body), string(eng.searchReqBody),
		"request body must be forwarded to the engine unmodified")
}

func TestESStore_GetUserRoomDoc_Found(t *testing.T) {
	eng := &stubEngine{
		docBody:  json.RawMessage(`{"_source":{"userAccount":"alice","rooms":["r1"],"restrictedRooms":{"rr":42}}}`),
		docFound: true,
	}
	s := newESStore(eng, "user-room")

	doc, found, err := s.GetUserRoomDoc(context.Background(), "alice")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "alice", doc.UserAccount)
	assert.Equal(t, []string{"r1"}, doc.Rooms)
	assert.Equal(t, map[string]int64{"rr": 42}, doc.RestrictedRooms)

	assert.Equal(t, "user-room", eng.docIndex)
	assert.Equal(t, "alice", eng.docDocID)
}

func TestESStore_GetUserRoomDoc_NotFound(t *testing.T) {
	eng := &stubEngine{docFound: false}
	s := newESStore(eng, "user-room")

	_, found, err := s.GetUserRoomDoc(context.Background(), "ghost")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestESStore_GetUserRoomDoc_TransportError(t *testing.T) {
	eng := &stubEngine{docErr: errors.New("boom")}
	s := newESStore(eng, "user-room")

	_, _, err := s.GetUserRoomDoc(context.Background(), "alice")
	assert.Error(t, err)
}

func TestESStore_GetUserRoomDoc_MalformedBody(t *testing.T) {
	eng := &stubEngine{docBody: json.RawMessage(`{not json`), docFound: true}
	s := newESStore(eng, "user-room")

	_, _, err := s.GetUserRoomDoc(context.Background(), "alice")
	assert.Error(t, err)
}

func TestESStore_UsesDefaultIndexWhenEmpty(t *testing.T) {
	eng := &stubEngine{docFound: false}
	s := newESStore(eng, "")

	_, _, err := s.GetUserRoomDoc(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, UserRoomIndex, eng.docIndex)
}
