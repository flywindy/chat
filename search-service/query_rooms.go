package main

import (
	"encoding/json"
	"fmt"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
)

// SpotlightIndex is the (local-only) room-typeahead index. Always one
// document per (user, room) pair — the spotlight index keeps search-as-you-
// type docs locally even when messages live on remote sites.
const SpotlightIndex = "spotlight"

// room scope filter values accepted on SearchRoomsRequest.Scope.
const (
	scopeAll     = "all"
	scopeChannel = "channel"
	scopeDM      = "dm"
	scopeApp     = "app"
)

// buildRoomQuery composes the ES `_search` body for a room search. It
// returns a *natsrouter.RouteError (user-facing) on invalid/unsupported
// scopes and a plain error on marshalling failures.
func buildRoomQuery(req model.SearchRoomsRequest, account string) (json.RawMessage, error) {
	scopeFilter, rerr := scopeFilterClause(req.Scope)
	if rerr != nil {
		return nil, rerr
	}

	filters := []any{
		map[string]any{"term": map[string]any{"userAccount": account}},
	}
	if scopeFilter != nil {
		filters = append(filters, scopeFilter)
	}

	body := map[string]any{
		"from":             req.Offset,
		"size":             req.Size,
		"track_total_hits": true,
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"multi_match": map[string]any{
							"query":    req.SearchText,
							"type":     "bool_prefix",
							"operator": "AND",
							"fields":   []string{"roomName"},
						},
					},
				},
				"filter": filters,
			},
		},
		"sort": []any{
			"_score",
			map[string]any{"joinedAt": map[string]any{"order": "desc"}},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal room query: %w", err)
	}
	return data, nil
}

// scopeFilterClause translates the request-level scope into an ES term
// filter on `roomType`. The filter values match the strings written to
// the spotlight index by search-sync-worker (the model.RoomType values
// themselves), NOT the Rocket.Chat legacy `p`/`d` convention. Returns
// (nil, nil) for "all" which needs no extra filter; returns an
// ErrBadRequest for `app` (MVP-unsupported) and for any unknown value.
func scopeFilterClause(scope string) (map[string]any, *natsrouter.RouteError) {
	switch scope {
	case "", scopeAll:
		return nil, nil
	case scopeChannel:
		return map[string]any{"term": map[string]any{"roomType": string(model.RoomTypeChannel)}}, nil
	case scopeDM:
		return map[string]any{"term": map[string]any{"roomType": string(model.RoomTypeDM)}}, nil
	case scopeApp:
		return nil, natsrouter.ErrBadRequest("scope=app not supported in MVP")
	default:
		return nil, natsrouter.ErrBadRequest(fmt.Sprintf("unknown scope: %s", scope))
	}
}
