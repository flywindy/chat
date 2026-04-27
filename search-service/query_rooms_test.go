package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
)

func roomFilters(t *testing.T, q map[string]any) []any {
	t.Helper()
	return q["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
}

func TestBuildRoomQuery_ScopeAll(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "general", Size: 10, Offset: 0}
	raw, err := buildRoomQuery(req, "alice")
	require.NoError(t, err)

	q := parseQuery(t, raw)
	assert.Equal(t, float64(0), q["from"])
	assert.Equal(t, float64(10), q["size"])
	assert.Equal(t, true, q["track_total_hits"])

	filters := roomFilters(t, q)
	require.Len(t, filters, 1)
	account := filters[0].(map[string]any)["term"].(map[string]any)["userAccount"]
	assert.Equal(t, "alice", account)
}

func TestBuildRoomQuery_ScopeExplicitAll(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "general", Scope: scopeAll}
	raw, err := buildRoomQuery(req, "alice")
	require.NoError(t, err)

	filters := roomFilters(t, parseQuery(t, raw))
	require.Len(t, filters, 1) // only userAccount, no roomType filter
}

func TestBuildRoomQuery_ScopeChannel(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "general", Scope: scopeChannel}
	raw, err := buildRoomQuery(req, "alice")
	require.NoError(t, err)

	filters := roomFilters(t, parseQuery(t, raw))
	require.Len(t, filters, 2)
	roomType := filters[1].(map[string]any)["term"].(map[string]any)["roomType"]
	assert.Equal(t, string(model.RoomTypeChannel), roomType)
}

func TestBuildRoomQuery_ScopeDM(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "alice", Scope: scopeDM}
	raw, err := buildRoomQuery(req, "alice")
	require.NoError(t, err)

	filters := roomFilters(t, parseQuery(t, raw))
	require.Len(t, filters, 2)
	roomType := filters[1].(map[string]any)["term"].(map[string]any)["roomType"]
	assert.Equal(t, string(model.RoomTypeDM), roomType)
}

func TestBuildRoomQuery_ScopeAppRejected(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "bot", Scope: scopeApp}
	_, err := buildRoomQuery(req, "alice")
	require.Error(t, err)

	var rerr *natsrouter.RouteError
	require.True(t, errors.As(err, &rerr), "expected RouteError")
	assert.Equal(t, natsrouter.CodeBadRequest, rerr.Code)
	assert.Contains(t, rerr.Message, "scope=app")
}

func TestBuildRoomQuery_UnknownScopeRejected(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "x", Scope: "orb"}
	_, err := buildRoomQuery(req, "alice")
	require.Error(t, err)

	var rerr *natsrouter.RouteError
	require.True(t, errors.As(err, &rerr))
	assert.Equal(t, natsrouter.CodeBadRequest, rerr.Code)
	assert.Contains(t, rerr.Message, "unknown scope")
}

func TestBuildRoomQuery_SortByScoreThenJoinedAtDesc(t *testing.T) {
	req := model.SearchRoomsRequest{SearchText: "x"}
	raw, err := buildRoomQuery(req, "alice")
	require.NoError(t, err)

	sort := parseQuery(t, raw)["sort"].([]any)
	require.Len(t, sort, 2)
	assert.Equal(t, "_score", sort[0])
	joinedAt := sort[1].(map[string]any)["joinedAt"].(map[string]any)
	assert.Equal(t, "desc", joinedAt["order"])
}
