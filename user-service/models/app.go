package models

import "github.com/hmchangw/chat/pkg/model"

// SetAppSubscriptionRequest is the body of subscription.setAppSubscription (PUT-like; Subscribed is the desired end-state).
type SetAppSubscriptionRequest struct {
	AppID      string `json:"appId"`
	Subscribed bool   `json:"subscribed"`
}

// AppListItem is an app plus the requesting user's subscription flag.
// `bson:",inline"` is REQUIRED: mongo-driver/v2 does NOT auto-inline anonymous structs (unlike encoding/json) — without it App fields decode empty.
type AppListItem struct {
	model.App    `bson:",inline"`
	IsSubscribed bool `json:"isSubscribed" bson:"isSubscribed"`
}

// AppsListRequest is the optional body of apps.list; server defaults/caps apply (default limit 20, max 100).
type AppsListRequest struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// AppsListResponse is returned by apps.list. hasMore is true when the next page
// would return at least one more app (the server over-fetches by one).
type AppsListResponse struct {
	Apps    []AppListItem `json:"apps"`
	HasMore bool          `json:"hasMore"`
}

// OKResponse is the generic success body (subscription.setAppSubscription).
type OKResponse struct {
	Success bool `json:"success"`
}
