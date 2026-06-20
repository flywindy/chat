package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/user-service/models"
)

var validListTypes = map[string]bool{"current": true, "rooms": true, "apps": true}

// maxSiteFanout bounds concurrent per-site room-service RPCs — otherwise a
// heavily-federated ALL_SITE_IDS fans one request into N simultaneous 5s RPCs.
const maxSiteFanout = 8

// DM-target markers rejected by GetDM: platform/system accounts are prefixed
// "p_" and bot accounts end in ".bot" — neither is a valid human DM counterpart.
const (
	dmTargetSystemPrefix = "p_"
	dmTargetBotSuffix    = ".bot"
)

// deletedRoomNamePrefix marks a soft-deleted room (room-service renames it to
// "Del-"+name); such rooms are surfaced on the subscription with no room object.
const deletedRoomNamePrefix = "Del-"

func (s *UserService) ListSubscriptions(c *natsrouter.Context, req models.SubscriptionListRequest) (*models.SubscriptionListResponse, error) {
	if !validListTypes[req.Type] {
		return nil, errcode.BadRequest("unknown subscription type")
	}
	if req.UpdatedWithinDays != nil && *req.UpdatedWithinDays < 0 {
		// A negative window computes a FUTURE cutoff and silently returns empty.
		return nil, errcode.BadRequest("updatedWithinDays must be non-negative")
	}
	account := c.Param("account")
	c.WithLogValues("account", account)
	subs, err := s.subs.AggregateSubscriptions(c, account, req.Type, req.UpdatedWithinDays, s.maxSubs)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	if req.Favorite != nil && *req.Favorite {
		subs = filterFavorites(subs)
		subs = moveSelfDMFront(subs, account)
	}
	s.enrichWithRoomInfo(c, subs)
	items := s.buildListItems(c, subs)
	return &models.SubscriptionListResponse{Subscriptions: items, Total: len(items)}, nil
}

// buildListItems wraps each enriched subscription into a heterogeneous list row:
//   - channel → base only
//   - botDM   → base + the nested app object; the base name is also swapped to
//     the app's display name (preserving the prior botDM-name behavior)
//   - dm      → base + the counterpart's hrInfo
//
// App and HR lookups degrade independently: a failed/missing lookup keeps the base
// name and omits the app object — it never fails the request.
func (s *UserService) buildListItems(c *natsrouter.Context, subs []model.Subscription) []model.SubscriptionItem {
	apps := s.lookupApps(c, subs)
	hrInfo := s.lookupHRInfo(c, subs)
	items := make([]model.SubscriptionItem, len(subs))
	for i := range subs {
		base := &subs[i]
		switch subs[i].RoomType {
		case model.RoomTypeBotDM:
			botDM := &model.BotDMSubscription{Subscription: base}
			if app, ok := apps[subs[i].Name]; ok && app != nil {
				if app.Name != "" {
					base.Name = app.Name
				}
				botDM.App = model.AppSubscriptionFromApp(app)
			}
			items[i] = botDM
		case model.RoomTypeDM:
			dm := &model.DMSubscription{Subscription: base}
			if hr, ok := hrInfo[subs[i].Name]; ok {
				dm.HRInfo = hr
			}
			items[i] = dm
		default:
			// channel / discussion rows ship the base Subscription unchanged.
			items[i] = &model.ChannelSubscription{Subscription: base}
		}
	}
	return items
}

// lookupApps fetches the full app docs for the distinct botDM bot accounts; a
// lookup failure degrades to nil (base name kept, no overlay).
func (s *UserService) lookupApps(c *natsrouter.Context, subs []model.Subscription) map[string]*model.App {
	bots := distinctNamesByType(subs, model.RoomTypeBotDM)
	if len(bots) == 0 {
		return nil
	}
	apps, err := s.apps.GetAppsByAssistants(c, bots)
	if err != nil {
		slog.WarnContext(c, "app metadata lookup degraded", "account", c.Param("account"), "request_id", natsutil.RequestIDFromContext(c), "error", err)
		return nil
	}
	return apps
}

// lookupHRInfo fetches the HR records for the distinct dm counterpart accounts; a
// lookup failure degrades to nil (no hrInfo).
func (s *UserService) lookupHRInfo(c *natsrouter.Context, subs []model.Subscription) map[string]*model.SubscriptionHRInfo {
	accounts := distinctNamesByType(subs, model.RoomTypeDM)
	if len(accounts) == 0 {
		return nil
	}
	hr, err := s.users.GetHRInfoByAccounts(c, accounts)
	if err != nil {
		slog.WarnContext(c, "hr info lookup degraded", "account", c.Param("account"), "request_id", natsutil.RequestIDFromContext(c), "error", err)
		return nil
	}
	return hr
}

// distinctNamesByType returns the deduplicated sub.Name values for rows of roomType.
func distinctNamesByType(subs []model.Subscription, roomType model.RoomType) []string {
	var names []string
	seen := map[string]struct{}{}
	for i := range subs {
		if subs[i].RoomType != roomType {
			continue
		}
		if _, dup := seen[subs[i].Name]; dup {
			continue
		}
		seen[subs[i].Name] = struct{}{}
		names = append(names, subs[i].Name)
	}
	return names
}

// enrichWithRoomInfo populates sub.Room for every subscription. LOCAL subs
// (subs[i].SiteID == s.siteID) are enriched entirely from local Mongo — the
// $lookup baseline carried on the subscription plus the room key read from the
// local rooms collection, with NO room-service RPC. Only CROSS-SITE subs fan out
// to the per-site GetRoomsInfo RPC, since their room docs live on another site.
//
// alert/hasMention are stored subscription state and are never touched here.
func (s *UserService) enrichWithRoomInfo(c *natsrouter.Context, subs []model.Subscription) {
	if len(subs) == 0 {
		return
	}

	// Partition by locality. Cross-site subs are further grouped per remote site
	// for the fan-out RPC.
	var localIdx []int
	idxBySite := map[string][]int{}
	for i := range subs {
		if subs[i].SiteID == s.siteID {
			localIdx = append(localIdx, i)
			continue
		}
		idxBySite[subs[i].SiteID] = append(idxBySite[subs[i].SiteID], i)
	}

	s.enrichLocal(subs, localIdx)
	s.enrichCrossSite(c, subs, idxBySite)
}

// enrichLocal builds sub.Room for LOCAL subs entirely from the $lookup baseline —
// room metadata plus the E2E key projected from the room's encKey sub-document —
// so it needs no separate key store read.
func (s *UserService) enrichLocal(subs []model.Subscription, localIdx []int) {
	for _, j := range localIdx {
		subs[j].Room = buildLocalRoom(&subs[j])
	}
}

// enrichCrossSite fans out per remote site to GetRoomsInfo; a failed site RPC
// leaves that site's subs without a room object (no baseline fallback — there is
// no local room doc for a cross-site room).
func (s *UserService) enrichCrossSite(c *natsrouter.Context, subs []model.Subscription, idxBySite map[string][]int) {
	if len(idxBySite) == 0 {
		return
	}
	sites := make([]string, 0, len(idxBySite))
	for site := range idxBySite {
		sites = append(sites, site)
	}
	infoBySite := make([]map[string]model.RoomInfo, len(sites)) // nil ⇒ site degraded
	// WaitGroup (not errgroup): errgroup.WithContext would cancel sibling site RPCs on the first error; per-site degradation must keep siblings running.
	// Acquire sem BEFORE spawning so live goroutine COUNT (not just concurrency) stays ≤ maxSiteFanout — a wide federation otherwise spawns one parked goroutine per site.
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxSiteFanout)
	for i, site := range sites {
		// Client already gone — stop firing further ~5s RPCs; the remaining sites
		// would only waste round-trips. In-flight calls fail fast via the ctx we
		// pass to GetRoomsInfo.
		if c.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Re-check after parking on the semaphore: cancellation may have
			// landed while this goroutine waited its turn behind earlier RPCs.
			if c.Err() != nil {
				return
			}
			roomIDs := make([]string, 0, len(idxBySite[site]))
			seen := make(map[string]struct{}, len(idxBySite[site]))
			for _, j := range idxBySite[site] {
				rid := subs[j].RoomID
				if _, dup := seen[rid]; dup {
					continue
				}
				seen[rid] = struct{}{}
				roomIDs = append(roomIDs, rid)
			}
			infos, err := s.rooms.GetRoomsInfo(c, site, roomIDs)
			if err != nil {
				slog.WarnContext(c, "room-info enrichment degraded", "account", c.Param("account"), "site", site, "request_id", natsutil.RequestIDFromContext(c), "error", err)
				return
			}
			m := make(map[string]model.RoomInfo, len(infos))
			for k := range infos {
				m[infos[k].RoomID] = infos[k]
			}
			infoBySite[i] = m
		}()
	}
	wg.Wait()
	for i, site := range sites {
		m := infoBySite[i]
		if m == nil {
			continue
		}
		for _, j := range idxBySite[site] {
			info := m[subs[j].RoomID]
			applyRoomInfo(&subs[j], &info)
		}
	}
}

// roomKeySecretLen is the AES-256-GCM key length. A baseline encKeyPriv of any
// other length is treated as absent (mirrors roomkeystore's secret validation).
const roomKeySecretLen = 32

// buildLocalRoom builds a SubscriptionRoom for a LOCAL sub entirely from its flat
// $lookup baseline — room metadata plus the E2E key projected from the room's
// encKey sub-document — so no separate key store read is needed. The baseline
// carries *time.Time, but the wire room object is epoch millis, so LastMsgAt/
// LastMentionAllAt are converted here (matching the cross-site RPC path).
func buildLocalRoom(sub *model.Subscription) *model.SubscriptionRoom {
	// A soft-deleted room (name "Del-...") is surfaced with no room object.
	if strings.HasPrefix(sub.RoomName, deletedRoomNamePrefix) {
		return nil
	}
	room := &model.SubscriptionRoom{
		SiteID:           sub.SiteID,
		Name:             sub.RoomName,
		UserCount:        sub.UserCount,
		AppCount:         sub.AppCount,
		LastMsgAt:        timeToMillis(sub.LastMsgAt),
		LastMsgID:        sub.LastMsgID,
		LastMentionAllAt: timeToMillis(sub.LastMentionAllAt),
	}
	if len(sub.RoomKeyPriv) == roomKeySecretLen {
		enc := base64.StdEncoding.EncodeToString(sub.RoomKeyPriv)
		ver := sub.RoomKeyVer
		room.PrivateKey = &enc
		room.KeyVersion = &ver
	}
	return room
}

// applyRoomInfo nests all room-derived fields (including the E2E key for initial
// key bootstrap) under sub.Room; zero-value info (Found=false) is skipped. The
// subscription's own fields are never overwritten — name, alert, and hasMention
// are authoritative subscription state; room-service only supplies room data.
func applyRoomInfo(sub *model.Subscription, info *model.RoomInfo) {
	if !info.Found {
		return
	}
	// Soft-deleted rooms (name "Del-...") surface with no room object.
	if strings.HasPrefix(info.Name, deletedRoomNamePrefix) {
		return
	}
	// info.LastMsgAt/LastMentionAllAt are already epoch millis (*int64) — the wire
	// room object uses the same representation, so they pass through directly.
	room := &model.SubscriptionRoom{
		SiteID:           info.SiteID,
		Name:             info.Name,
		UserCount:        info.UserCount,
		AppCount:         info.AppCount,
		LastMsgAt:        info.LastMsgAt,
		LastMsgID:        info.LastMsgID,
		LastMentionAllAt: info.LastMentionAllAt,
		PrivateKey:       info.PrivateKey,
		KeyVersion:       info.KeyVersion,
	}
	sub.Room = room
}

// timeToMillis converts a nullable UTC timestamp to epoch millis for the wire
// room object; nil in ⇒ nil out (the field is omitted).
func timeToMillis(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	ms := t.UTC().UnixMilli()
	return &ms
}

// unread: a room event at ms (epoch millis) is newer than lastSeen; nil ms ⇒ false, nil lastSeen with ms set ⇒ true.
func unread(lastSeen *time.Time, ms *int64) bool {
	if ms == nil {
		return false
	}
	if lastSeen == nil {
		return true
	}
	return lastSeen.UTC().UnixMilli() < *ms
}

func filterFavorites(subs []model.Subscription) []model.Subscription {
	// [:0:0] — cap 0 forces a fresh backing array so append never aliases/mutates the input
	out := subs[:0:0]
	for i := range subs {
		if subs[i].Favorite {
			out = append(out, subs[i])
		}
	}
	return out
}

func moveSelfDMFront(subs []model.Subscription, account string) []model.Subscription {
	for i := range subs {
		if subs[i].RoomType == model.RoomTypeDM && subs[i].Name == account {
			out := make([]model.Subscription, 0, len(subs))
			out = append(out, subs[i])
			out = append(out, subs[:i]...)
			return append(out, subs[i+1:]...)
		}
	}
	return subs
}

func (s *UserService) GetChannels(c *natsrouter.Context, req models.GetChannelsRequest) (*models.SubscriptionListResponse, error) {
	account := c.Param("account")
	c.WithLogValues("account", account)
	hasContain, hasNames := req.MembersContain != "", len(req.AccountNames) > 0
	if hasContain == hasNames {
		return nil, errcode.BadRequest("exactly one of membersContain or accountNames is required")
	}
	// maxAccountNames caps getChannels' accountNames — unbounded input builds an arbitrarily large $in/$setIsSubset operand.
	if len(req.AccountNames) > s.maxAccountNames {
		return nil, errcode.BadRequest("too many accountNames")
	}
	members := req.AccountNames
	if hasContain {
		members = []string{req.MembersContain}
	}
	subs, err := s.subs.FindChannelsByMembers(c, account, members, s.maxSubs)
	if err != nil {
		return nil, fmt.Errorf("get channels: %w", err)
	}
	s.enrichWithRoomInfo(c, subs)
	items := s.buildListItems(c, subs)
	return &models.SubscriptionListResponse{Subscriptions: items, Total: len(items)}, nil
}

func (s *UserService) GetDM(c *natsrouter.Context, req models.GetDMRequest) (*models.DMResponse, error) {
	account := c.Param("account")
	c.WithLogValues("account", account, "target", req.AccountName)
	if req.AccountName == "" {
		return nil, errcode.BadRequest("accountName required")
	}
	if strings.HasPrefix(req.AccountName, dmTargetSystemPrefix) || strings.HasSuffix(req.AccountName, dmTargetBotSuffix) {
		return nil, errcode.BadRequest("invalid DM target", errcode.WithReason(errcode.UserInvalidDMTarget))
	}
	dm, err := s.subs.GetDMSubscription(c, account, req.AccountName)
	if err != nil {
		return nil, fmt.Errorf("get dm: %w", err)
	}
	if dm == nil {
		return nil, errcode.NotFound("dm not found", errcode.WithReason(errcode.UserSubscriptionNotFound))
	}
	if dm.Subscription == nil {
		return nil, errcode.Internal("malformed dm subscription")
	}
	// enrichWithRoomInfo mutates slice elements in place, so the single embedded sub is boxed into a 1-elem slice to receive the update.
	one := []model.Subscription{*dm.Subscription}
	s.enrichWithRoomInfo(c, one)
	out := *dm
	out.Subscription = &one[0]
	return &models.DMResponse{Subscription: out}, nil
}

// GetByRoomID returns the caller's room-info-enriched subscription for req.RoomID
// as a 0-or-1-element list (empty = not subscribed; absence is a normal answer).
func (s *UserService) GetByRoomID(c *natsrouter.Context, req models.GetByRoomIDRequest) (*models.SubscriptionListResponse, error) {
	account := c.Param("account")
	c.WithLogValues("account", account, "roomId", req.RoomID)
	if req.RoomID == "" {
		return nil, errcode.BadRequest("roomId required")
	}
	sub, err := s.subs.GetSubscriptionByRoomID(c, account, req.RoomID)
	if err != nil {
		return nil, fmt.Errorf("get subscription by roomId: %w", err)
	}
	if sub == nil {
		return &models.SubscriptionListResponse{Subscriptions: []model.SubscriptionItem{}, Total: 0}, nil
	}
	one := []model.Subscription{*sub}
	s.enrichWithRoomInfo(c, one)
	items := s.buildListItems(c, one)
	return &models.SubscriptionListResponse{Subscriptions: items, Total: len(items)}, nil
}

func (s *UserService) CountSubscriptions(c *natsrouter.Context, req models.CountRequest) (*models.CountResponse, error) {
	account := c.Param("account")
	c.WithLogValues("account", account)
	total, err := s.subs.CountActiveSubscriptions(c, account)
	if err != nil {
		return nil, fmt.Errorf("count subscriptions: %w", err)
	}
	if req.Unread == nil || !*req.Unread {
		return &models.CountResponse{Count: total}, nil
	}
	return s.countUnread(c, account, total)
}

// countUnread counts active subs with unread messages. LOCAL subs are counted from the
// $lookup baseline (room.lastMsgAt) with no RPC; CROSS-SITE subs use per-site GetRoomsInfo
// RPCs (fail-fast) — any cross-site failure falls back to total, since a partial count would mislead.
func (s *UserService) countUnread(ctx context.Context, account string, total int) (*models.CountResponse, error) {
	// Short-circuit zero: min(0, maxSubs)=0 would build a $limit:0 MongoDB rejects.
	if total == 0 {
		return &models.CountResponse{Count: 0}, nil
	}
	// Cap at maxSubs — query-side total can exceed the cap; min keeps the fetch bounded and consistent with the list endpoints.
	subs, err := s.subs.GetActiveSubscriptions(ctx, account, min(total, s.maxSubs))
	if err != nil {
		return nil, fmt.Errorf("count unread: %w", err)
	}

	// LOCAL subs carry room.lastMsgAt on the $lookup baseline — count them with no RPC.
	// Only CROSS-SITE subs need the per-site GetRoomsInfo RPC (their room docs live remotely).
	unreadTotal := 0
	crossBySite := map[string][]model.Subscription{}
	for i := range subs {
		if subs[i].SiteID == s.siteID {
			if unread(subs[i].LastSeenAt, timeToMillis(subs[i].LastMsgAt)) {
				unreadTotal++
			}
			continue
		}
		crossBySite[subs[i].SiteID] = append(crossBySite[subs[i].SiteID], subs[i])
	}
	if len(crossBySite) == 0 {
		return &models.CountResponse{Count: unreadTotal}, nil
	}

	sites := make([]string, 0, len(crossBySite))
	for site := range crossBySite {
		sites = append(sites, site)
	}
	results := make([]int, len(sites))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxSiteFanout) // bound concurrent per-site RPCs
	for i, site := range sites {
		g.Go(func() error {
			siteSubs := crossBySite[site]
			roomIDs := make([]string, 0, len(siteSubs))
			seenRooms := make(map[string]struct{}, len(siteSubs))
			for j := range siteSubs {
				rid := siteSubs[j].RoomID
				if _, dup := seenRooms[rid]; dup {
					continue
				}
				seenRooms[rid] = struct{}{}
				roomIDs = append(roomIDs, rid)
			}
			infos, err := s.rooms.GetRoomsInfo(gctx, site, roomIDs)
			if err != nil {
				return fmt.Errorf("unread count rooms-info for site %s: %w", site, err)
			}
			lastMsg := make(map[string]*int64, len(infos))
			for k := range infos {
				lastMsg[infos[k].RoomID] = infos[k].LastMsgAt
			}
			n := 0
			for j := range siteSubs {
				if unread(siteSubs[j].LastSeenAt, lastMsg[siteSubs[j].RoomID]) {
					n++
				}
			}
			results[i] = n
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		slog.WarnContext(ctx, "unread count fell back to total", "account", account, "request_id", natsutil.RequestIDFromContext(ctx), "error", err)
		return &models.CountResponse{Count: total}, nil
	}
	for _, n := range results {
		unreadTotal += n
	}
	return &models.CountResponse{Count: unreadTotal}, nil
}
