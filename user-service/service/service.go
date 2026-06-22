package service

import (
	"context"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/mongoutil"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/subject"
	"github.com/hmchangw/chat/user-service/config"
	"github.com/hmchangw/chat/user-service/models"
)

//go:generate mockgen -destination=mocks/mock_repository.go -package=mocks . SubscriptionRepository,UserRepository,AppRepository,RoomClient,EventPublisher

// SubscriptionRepository is the consumer-defined interface for subscription persistence (botDM app-subscription rows included).
type SubscriptionRepository interface {
	AggregateSubscriptions(ctx context.Context, account, listType string, withinDays *int, limit int) ([]model.Subscription, error)
	FindChannelsByMembers(ctx context.Context, account string, members []string, limit int) ([]model.Subscription, error)
	GetDMSubscription(ctx context.Context, account, target string) (*model.DMSubscription, error)
	GetSubscriptionByRoomID(ctx context.Context, account, roomID string) (*model.Subscription, error)
	CountActiveSubscriptions(ctx context.Context, account string) (int, error)
	GetActiveSubscriptions(ctx context.Context, account string, limit int) ([]model.Subscription, error)
	GetAppSubscription(ctx context.Context, account, botName string) (*model.Subscription, error)
	SetAppSubscribed(ctx context.Context, account, botName string, subscribed, muted bool) error
}

// UserRepository is the consumer-defined interface for user status persistence.
type UserRepository interface {
	GetUserStatus(ctx context.Context, account string) (*model.User, error)
	SetUserStatus(ctx context.Context, account, text string, isShow *bool) (*model.User, error)
	GetHRInfoByAccounts(ctx context.Context, accounts []string) (map[string]*model.SubscriptionHRInfo, error)
}

// AppRepository is the consumer-defined interface for app catalog reads.
type AppRepository interface {
	GetApp(ctx context.Context, appID string) (*model.App, error)
	ListApps(ctx context.Context, account string, page mongoutil.OffsetPageRequest) (mongoutil.OffsetPage[models.AppListItem], error)
	GetAppsByAssistants(ctx context.Context, botAccounts []string) (map[string]*model.App, error)
}

// RoomClient is the consumer-defined interface for room-service RPC calls.
type RoomClient interface {
	GetRoomsInfo(ctx context.Context, siteID string, roomIDs []string) ([]model.RoomInfo, error)
	CreateDMRoom(ctx context.Context, account, otherAccount string, roomType model.RoomType) (model.Subscription, error)
}

// EventPublisher is the consumer-defined interface for fire-and-forget
// federation publishing — a JetStream publish directly into the destination
// site's INBOX stream. Status is last-write-wins and idempotent, so no
// msgID/dedup is needed.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// UserService handles all user-related NATS request/reply endpoints.
type UserService struct {
	subs            SubscriptionRepository
	users           UserRepository
	apps            AppRepository
	rooms           RoomClient
	pub             EventPublisher
	siteID          string
	allSiteIDs      []string
	maxSubs         int
	maxAccountNames int
}

// New constructs a UserService with the given dependencies and configuration.
func New(subs SubscriptionRepository, users UserRepository, apps AppRepository, rooms RoomClient, pub EventPublisher, cfg *config.Config) *UserService {
	return &UserService{
		subs:            subs,
		users:           users,
		apps:            apps,
		rooms:           rooms,
		pub:             pub,
		siteID:          cfg.SiteID,
		allSiteIDs:      cfg.AllSiteIDs,
		maxSubs:         cfg.MaxSubscriptionLimit,
		maxAccountNames: cfg.MaxAccountNames,
	}
}

// RegisterHandlers wires all UserService endpoints onto the router.
// siteID is a literal token in each pattern — this instance only subscribes to its own siteID subjects.
func (s *UserService) RegisterHandlers(r *natsrouter.Router) {
	natsrouter.Register(r, subject.UserStatusGetByNamePattern(s.siteID), s.GetStatusByName)
	natsrouter.Register(r, subject.UserProfileGetByNamePattern(s.siteID), s.GetProfileByName)
	natsrouter.Register(r, subject.UserStatusSetPattern(s.siteID), s.SetStatus)
	natsrouter.Register(r, subject.UserSubscriptionListPattern(s.siteID), s.ListSubscriptions)
	natsrouter.Register(r, subject.UserSubscriptionGetChannelsPattern(s.siteID), s.GetChannels)
	natsrouter.Register(r, subject.UserSubscriptionGetDMPattern(s.siteID), s.GetDM)
	natsrouter.Register(r, subject.UserSubscriptionGetByRoomIDPattern(s.siteID), s.GetByRoomID)
	natsrouter.Register(r, subject.UserSubscriptionCountPattern(s.siteID), s.CountSubscriptions)
	natsrouter.Register(r, subject.UserSubscriptionSetAppSubscriptionPattern(s.siteID), s.SetAppSubscription)
	natsrouter.Register(r, subject.UserAppsListPattern(s.siteID), s.ListApps)
}
