package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// UserRoomIndex is the default index holding per-user access-control docs.
// The sync-worker uses a site-qualified name internally, but the search
// service reaches it via a stable alias — one name across cluster
// topologies. Overridable via SEARCH_USER_ROOM_INDEX.
const UserRoomIndex = "user-room"

// esEngine is the narrow slice of pkg/searchengine.SearchEngine the
// store uses — declared at the consumer so unit tests can stub without
// satisfying the full SearchEngine contract.
type esEngine interface {
	Search(ctx context.Context, indices []string, body json.RawMessage) (json.RawMessage, error)
	GetDoc(ctx context.Context, index, docID string) (json.RawMessage, bool, error)
}

type esStore struct {
	engine        esEngine
	userRoomIndex string
}

func newESStore(engine esEngine, userRoomIndex string) *esStore {
	return &esStore{engine: engine, userRoomIndex: resolveUserRoomIndex(userRoomIndex)}
}

// resolveUserRoomIndex falls back to UserRoomIndex when empty. Kept as a
// single normalization point so both newESStore and termsLookupClause
// consult the same default without repeating the `if == ""` branch.
func resolveUserRoomIndex(name string) string {
	if name == "" {
		return UserRoomIndex
	}
	return name
}

func (s *esStore) Search(ctx context.Context, indices []string, body json.RawMessage) (json.RawMessage, error) {
	raw, err := s.engine.Search(ctx, indices, body)
	if err != nil {
		return nil, fmt.Errorf("es store search: %w", err)
	}
	return raw, nil
}

// GetUserRoomDoc fetches the access-control doc. On a 404 (doc absent or
// index missing) returns (zero, false, nil) so the handler can populate an
// empty-map cache entry instead of erroring.
func (s *esStore) GetUserRoomDoc(ctx context.Context, account string) (UserRoomDoc, bool, error) {
	raw, found, err := s.engine.GetDoc(ctx, s.userRoomIndex, account)
	if err != nil {
		return UserRoomDoc{}, false, fmt.Errorf("get user-room doc: %w", err)
	}
	if !found {
		return UserRoomDoc{}, false, nil
	}

	// ES wraps the document in `{ _source: … }`; extract it.
	var wrapper struct {
		Source UserRoomDoc `json:"_source"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return UserRoomDoc{}, false, fmt.Errorf("decode user-room doc: %w", err)
	}
	return wrapper.Source, true, nil
}
