package main

import (
	"context"

	"github.com/hmchangw/chat/pkg/model"
)

//go:generate mockgen -source=store.go -destination=mock_store_test.go -package=main

// avatarStore is the data access this service needs. Each method reads/writes
// exactly one collection: users, subscriptions, or avatars.
type avatarStore interface {
	// EmployeeID returns a user's employeeId (users collection). found=false when
	// the account has no user record or no employeeId.
	EmployeeID(ctx context.Context, account string) (eid string, found bool, err error)
	// BotSite returns a bot's owning siteID from its user record (bots are users,
	// synced to every cluster). found=false when no such bot record exists.
	BotSite(ctx context.Context, account string) (siteID string, found bool, err error)
	// RoomSite returns the room's owning site, type, and name from any one of its
	// local subscriptions. found=false when no local subscription exists.
	RoomSite(ctx context.Context, roomID string) (siteID string, roomType model.RoomType, name string, found bool, err error)
	// Avatar looks up a custom-image doc by subject. found=false → serve default.
	Avatar(ctx context.Context, subjectType model.AvatarSubjectType, subjectID string) (*model.Avatar, bool, error)
	// SetBotAvatar upserts a bot's avatars doc (by _id).
	SetBotAvatar(ctx context.Context, av *model.Avatar) error
}
