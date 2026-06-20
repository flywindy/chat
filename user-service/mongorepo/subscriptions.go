package mongorepo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/mongoutil"
)

const subscriptionsCollection = "subscriptions"

// roomsCollection is the $lookup target for the deleted-filter and enrichment; owned by room-service, referenced only by name.
const roomsCollection = "rooms"

// deletedRoomNameRegex matches room-service's soft-delete rename ("Del-"+name); the deleted-filter excludes matching local subs.
const deletedRoomNameRegex = "^Del-"

// SubscriptionRepo is the Mongo implementation of service.SubscriptionRepository.
type SubscriptionRepo struct {
	subscriptions *mongoutil.Collection[model.Subscription]
	siteID        string // this instance's site — distinguishes local vs cross-site rows in the deleted-filter
}

// NewSubscriptionRepo builds a SubscriptionRepo over db; the deleted-filter keeps cross-site rows, drops local rows with missing/soft-deleted rooms.
func NewSubscriptionRepo(db *mongo.Database, siteID string) *SubscriptionRepo {
	return &SubscriptionRepo{
		subscriptions: mongoutil.NewCollection[model.Subscription](db.Collection(subscriptionsCollection)),
		siteID:        siteID,
	}
}

// EnsureIndexes creates the subscription indexes this service queries on.
func (r *SubscriptionRepo) EnsureIndexes(ctx context.Context) error {
	if _, err := r.subscriptions.Raw().Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Serves the account+roomType match on every list/count path; the retention
		// window keys on room.lastMsgAt (a room field), so no trailing time key.
		{Keys: bson.D{{Key: "u.account", Value: 1}, {Key: "roomType", Value: 1}}},
		// Unique logical key (one subscription per room per user). Must match
		// room-service's declaration on the shared collection (mismatch → conflict).
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "u.account", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "name", Value: 1}, {Key: "roomType", Value: 1}}},
	}); err != nil {
		return fmt.Errorf("create subscription indexes: %w", err)
	}
	return nil
}

// roomsEnrichStages builds the shared rooms-join + enrichment. When dropDeleted is true
// (count/active paths) it also drops local soft-deleted (^Del-) rooms; the list paths
// pass false and keep them (the service nulls their room object). A missing/cross-site
// room has no room.name so it is kept either way. The activity window is applied
// separately by the caller on the subscription's own _updatedAt.
func roomsEnrichStages(dropDeleted bool) bson.A {
	stages := bson.A{
		bson.M{"$lookup": bson.M{"from": roomsCollection, "localField": "roomId", "foreignField": "_id", "as": "room"}},
		bson.M{"$unwind": bson.M{"path": "$room", "preserveNullAndEmptyArrays": true}},
	}
	if dropDeleted {
		// A local Del- room.name matches the regex → inverted by $not → dropped.
		stages = append(stages, bson.M{"$match": bson.M{"room.name": bson.M{"$not": bson.M{"$regex": deletedRoomNameRegex}}}})
	}
	return append(stages,
		bson.M{"$addFields": bson.M{
			"userCount":        "$room.userCount",
			"lastMsgAt":        "$room.lastMsgAt",
			"lastMsgId":        "$room.lastMsgId",
			"lastMentionAllAt": "$room.lastMentionAllAt",
			"appCount":         "$room.appCount",
			"roomName":         "$room.name",
			// Sort key: room activity (lastMsgAt), falling back to room.createdAt for
			// rooms with no messages. Null for cross-site/missing rooms (they sort last).
			"__sortKey": bson.M{"$ifNull": bson.A{"$room.lastMsgAt", "$room.createdAt"}},
			// Room E2E key baseline (current slot) for local enrichment — folds the
			// key read into this single $lookup, no separate keystore round-trip.
			"encKeyPriv": "$room.encKey.priv",
			"encKeyVer":  "$room.encKey.ver",
		}},
		bson.M{"$project": bson.M{"room": 0}},
	)
}

// matchedRoomField is the scratch array the member-match pipeline joins the local
// room into; stripped by subscriptionProjection before the result decodes.
const matchedRoomField = "__matchedRoom"

// roomMatchStages joins the local rooms collection into the matchedRoomField array
// — excluding soft-deleted (^Del-) rooms inside the $lookup — then drops any sub
// whose room is missing/deleted (empty array, via $ne: []). It runs BEFORE the
// heavier co-member self-join so the cheap room filter shrinks the candidate set
// first. Unlike roomsEnrichStages this DROPS missing/cross-site rooms (no local
// room doc ⇒ empty array): member matching is inherently local.
func roomMatchStages() []bson.D {
	return []bson.D{
		{{Key: "$lookup", Value: bson.M{
			"from": roomsCollection,
			"let":  bson.M{"rid": "$roomId"},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{
					"$expr": bson.M{"$eq": bson.A{"$_id", "$$rid"}},
					"name":  bson.M{"$not": bson.M{"$regex": deletedRoomNameRegex}},
				}},
			},
			"as": matchedRoomField,
		}}},
		{{Key: "$match", Value: bson.M{matchedRoomField: bson.M{"$ne": bson.A{}}}}},
	}
}

// subscriptionProjection is the terminal $project for the member-match pipeline:
// an inclusion projection of the subscription's fields (incl. the room baseline
// copied to the top level). Being inclusion-only, it naturally drops the
// pipeline's scratch arrays (__matchedRoom, __members, __memberAccounts). extra adds
// further caller-named fields.
func subscriptionProjection(extra bson.M) bson.M {
	proj := bson.M{
		"_id":                1,
		"u":                  1,
		"roomId":             1,
		"siteId":             1,
		"roles":              1,
		"name":               1,
		"roomType":           1,
		"isSubscribed":       1,
		"historySharedSince": 1,
		"joinedAt":           1,
		"lastSeenAt":         1,
		"hasMention":         1,
		"hasGroupMention":    1,
		"hasUnread":          1,
		"threadUnread":       1,
		"alert":              1,
		"muted":              1,
		"favorite":           1,
		"restricted":         1,
		"externalAccess":     1,
		"avatarUrl":          1,
		"favoritedAt":        1,
		"_updatedAt":         1, // subscription's Mongo field (wire: updatedAt)
		// room baseline copied to the top level (consumed by local enrichment)
		"userCount":        1,
		"lastMsgAt":        1,
		"lastMsgId":        1,
		"lastMentionAllAt": 1,
		"appCount":         1,
		"roomName":         1,
		"encKeyPriv":       1,
		"encKeyVer":        1,
	}
	for k, v := range extra {
		proj[k] = v
	}
	return proj
}

// dedupeStrings returns in with duplicates removed, preserving first-seen order.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// AggregateSubscriptions lists account's subscriptions by listType: rooms (dm+channel), apps (subscribed botDMs), or current (merged $facet set).
func (r *SubscriptionRepo) AggregateSubscriptions(ctx context.Context, account, listType string, withinDays *int, limit int) ([]model.Subscription, error) {
	if listType == "current" {
		return r.aggregateCurrent(ctx, account, limit)
	}
	match := bson.M{"u.account": account}
	switch listType {
	case "rooms":
		match["roomType"] = bson.M{"$in": bson.A{"dm", "channel"}}
		if withinDays != nil {
			// Window on the subscription's own _updatedAt, not on room activity.
			within := time.Now().UTC().AddDate(0, 0, -*withinDays)
			if !within.IsZero() {
				match["_updatedAt"] = bson.M{"$gte": within}
			}
		}
	case "apps":
		// withinDays is intentionally not applied to apps subscriptions.
		match["roomType"] = "botDM"
		match["isSubscribed"] = true
	}
	pipeline := bson.A{bson.M{"$match": match}}
	pipeline = append(pipeline, roomsEnrichStages(false)...)
	pipeline = append(pipeline,
		bson.M{"$sort": bson.D{{Key: "__sortKey", Value: -1}, {Key: "name", Value: 1}}},
		bson.M{"$limit": int64(limit)},
	)
	return r.subscriptions.Aggregate(ctx, pipeline)
}

// aggregateCurrent merges the rooms (dm/channel) and apps (botDM) $facet branches — each needs a different roomType $match; no window.
func (r *SubscriptionRepo) aggregateCurrent(ctx context.Context, account string, limit int) ([]model.Subscription, error) {
	match := bson.M{"u.account": account, "$or": bson.A{
		bson.M{"roomType": bson.M{"$in": bson.A{"dm", "channel"}}},
		bson.M{"roomType": "botDM", "isSubscribed": true},
	}}
	pipeline := bson.A{bson.M{"$match": match}}
	pipeline = append(pipeline, roomsEnrichStages(false)...)
	sortStage := bson.M{"$sort": bson.D{{Key: "__sortKey", Value: -1}, {Key: "name", Value: 1}}}
	limitStage := bson.M{"$limit": int64(limit)}
	pipeline = append(pipeline,
		bson.M{"$facet": bson.M{
			// Branches sort+limit BEFORE the merge so the post-concat $sort sees
			// ≤ 2*limit docs — the global top-K is within the union of branch top-Ks.
			"rooms": bson.A{
				bson.M{"$match": bson.M{"roomType": bson.M{"$in": bson.A{"dm", "channel"}}}},
				sortStage, limitStage,
			},
			"apps": bson.A{
				bson.M{"$match": bson.M{"roomType": "botDM"}},
				sortStage, limitStage,
			},
		}},
		bson.M{"$project": bson.M{"all": bson.M{"$concatArrays": bson.A{"$rooms", "$apps"}}}},
		bson.M{"$unwind": "$all"},
		bson.M{"$replaceRoot": bson.M{"newRoot": "$all"}},
		sortStage,
		limitStage,
	)
	return r.subscriptions.Aggregate(ctx, pipeline)
}

// FindChannelsByMembers returns the requester's channel subs whose room contains the requester and ALL given members (bots excluded by the ".bot" suffix), room.createdAt desc.
// The room match (roomMatchStages) runs first so the deleted/missing filter shrinks the set before the co-member self-join.
func (r *SubscriptionRepo) FindChannelsByMembers(ctx context.Context, account string, members []string, limit int) ([]model.Subscription, error) {
	members = dedupeStrings(members)
	// allAccounts is the full set the room must contain: the requested members plus
	// the requester. Bots (".bot" accounts) are excluded in the co-member join below,
	// so a bot passed in members can never satisfy the match.
	allAccounts := dedupeStrings(append(append([]string{}, members...), account))
	pipeline := bson.A{
		bson.M{"$match": bson.M{"u.account": account, "roomType": "channel"}},
	}
	for _, st := range roomMatchStages() {
		pipeline = append(pipeline, st)
	}
	pipeline = append(pipeline,
		// Co-member self-join — NOT siteId-filtered (any local/federated sub counts),
		// projected to u.account only. Bots are excluded by the ".bot" account suffix.
		// allAccounts is $literal-wrapped so $-values read as literals, not field paths.
		bson.M{"$lookup": bson.M{
			"from": subscriptionsCollection,
			"let":  bson.M{"rid": "$roomId"},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$roomId", "$$rid"}},
					bson.M{"$not": bson.M{"$regexMatch": bson.M{"input": "$u.account", "regex": "\\.bot$"}}},
					bson.M{"$in": bson.A{"$u.account", bson.M{"$literal": allAccounts}}},
				}}}},
				bson.M{"$project": bson.M{"_id": 0, "u.account": 1}},
			},
			"as": "__members",
		}},
		// Require every account present: $all (subset) + $size (exact count). The unique
		// (roomId, u.account) index gives one row per account, so the mapped accounts are
		// already distinct — no $setUnion needed.
		bson.M{"$addFields": bson.M{"__memberAccounts": bson.M{"$map": bson.M{
			"input": "$__members", "as": "m", "in": "$$m.u.account",
		}}}},
		bson.M{"$match": bson.M{"__memberAccounts": bson.M{"$all": allAccounts, "$size": len(allAccounts)}}},
		// Copy the matched room's baseline to the top level (consumed by local enrichment).
		bson.M{"$addFields": bson.M{
			"userCount":        bson.M{"$first": "$" + matchedRoomField + ".userCount"},
			"lastMsgAt":        bson.M{"$first": "$" + matchedRoomField + ".lastMsgAt"},
			"lastMsgId":        bson.M{"$first": "$" + matchedRoomField + ".lastMsgId"},
			"lastMentionAllAt": bson.M{"$first": "$" + matchedRoomField + ".lastMentionAllAt"},
			"appCount":         bson.M{"$first": "$" + matchedRoomField + ".appCount"},
			"roomName":         bson.M{"$first": "$" + matchedRoomField + ".name"},
			// Room E2E key baseline (current slot) — folds the key read into this join.
			"encKeyPriv": bson.M{"$first": "$" + matchedRoomField + ".encKey.priv"},
			"encKeyVer":  bson.M{"$first": "$" + matchedRoomField + ".encKey.ver"},
		}},
		bson.M{"$sort": bson.D{{Key: matchedRoomField + ".createdAt", Value: -1}}},
		bson.M{"$limit": int64(limit)},
		bson.D{{Key: "$project", Value: subscriptionProjection(nil)}},
	)
	return r.subscriptions.Aggregate(ctx, pipeline)
}

// GetDMSubscription returns the requester's room-enriched DM sub with target plus the counterpart's HRInfo (cross-site ⇒ nil), or (nil, nil).
func (r *SubscriptionRepo) GetDMSubscription(ctx context.Context, account, target string) (*model.DMSubscription, error) {
	pipeline := bson.A{
		bson.M{"$match": bson.M{"u.account": account, "name": target, "roomType": "dm"}},
		bson.M{"$limit": int64(1)}, // (account, name, roomType=dm) is unique — short-circuit defensively
	}
	pipeline = append(pipeline, roomsEnrichStages(false)...)
	pipeline = append(pipeline,
		bson.M{"$lookup": bson.M{"from": usersCollection, "localField": "name", "foreignField": "account", "as": "hrUser"}},
		bson.M{"$unwind": bson.M{"path": "$hrUser", "preserveNullAndEmptyArrays": true}},
		bson.M{"$addFields": bson.M{"hrInfo": bson.M{"$cond": bson.A{
			bson.M{"$ifNull": bson.A{"$hrUser", false}},
			bson.M{
				"account": "$hrUser.account",
				// HRInfo.Name carries the Chinese (native) name — User has no plain "name".
				"name":    "$hrUser.chineseName",
				"engName": "$hrUser.engName",
			},
			"$$REMOVE",
		}}}},
		bson.M{"$project": bson.M{"hrUser": 0}},
	)
	// .Raw(): decodes into []model.DMSubscription, not []model.Subscription.
	cur, err := r.subscriptions.Raw().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregate dm subscription: %w", err)
	}
	var out []model.DMSubscription
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode dm subscription: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// GetSubscriptionByRoomID returns the requester's deleted-filtered sub for roomID, or (nil, nil); (account, roomId) is unique in practice.
func (r *SubscriptionRepo) GetSubscriptionByRoomID(ctx context.Context, account, roomID string) (*model.Subscription, error) {
	pipeline := bson.A{bson.M{"$match": bson.M{"u.account": account, "roomId": roomID}}}
	pipeline = append(pipeline, roomsEnrichStages(false)...)
	pipeline = append(pipeline, bson.M{"$limit": int64(1)}) // (roomId, u.account) is unique — short-circuit defensively
	out, err := r.subscriptions.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregate subscription by roomId: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// activeSubscriptionFilter: non-muted dm/channel subs, or non-muted subscribed botDMs (the count
// endpoints' notion of active). Unlike the list endpoints, the count EXCLUDES muted subs — mute
// keeps a room visible in lists but out of the active/badge count.
func activeSubscriptionFilter(account string) bson.M {
	return bson.M{"u.account": account, "muted": bson.M{"$ne": true}, "$or": bson.A{
		bson.M{"roomType": bson.M{"$in": bson.A{"dm", "channel"}}},
		bson.M{"roomType": "botDM", "isSubscribed": true},
	}}
}

// CountActiveSubscriptions counts the deleted-filtered active set via $count over the enriched pipeline (CountDocuments cannot see the join).
func (r *SubscriptionRepo) CountActiveSubscriptions(ctx context.Context, account string) (int, error) {
	pipeline := bson.A{bson.M{"$match": activeSubscriptionFilter(account)}}
	pipeline = append(pipeline, roomsEnrichStages(true)...)
	pipeline = append(pipeline, bson.M{"$count": "n"})
	cur, err := r.subscriptions.Raw().Aggregate(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("count active subscriptions: %w", err)
	}
	var out []struct {
		N int `bson:"n"`
	}
	if err := cur.All(ctx, &out); err != nil {
		return 0, fmt.Errorf("decode active subscription count: %w", err)
	}
	if len(out) == 0 {
		return 0, nil
	}
	return out[0].N, nil
}

// GetActiveSubscriptions returns the deleted-filtered active set used by the unread count, capped by limit.
func (r *SubscriptionRepo) GetActiveSubscriptions(ctx context.Context, account string, limit int) ([]model.Subscription, error) {
	pipeline := bson.A{bson.M{"$match": activeSubscriptionFilter(account)}}
	pipeline = append(pipeline, roomsEnrichStages(true)...)
	// MongoDB rejects $limit:0 — callers short-circuit zero; stay defensive here.
	if limit > 0 {
		pipeline = append(pipeline, bson.M{"$limit": int64(limit)})
	}
	return r.subscriptions.Aggregate(ctx, pipeline)
}

// GetAppSubscription returns the requester's botDM subscription for botName, or (nil, nil).
func (r *SubscriptionRepo) GetAppSubscription(ctx context.Context, account, botName string) (*model.Subscription, error) {
	return r.subscriptions.FindOne(ctx, bson.M{"u.account": account, "name": botName, "roomType": "botDM"})
}

// SetAppSubscribed updates isSubscribed/muted on the requester's botDM subscription.
func (r *SubscriptionRepo) SetAppSubscribed(ctx context.Context, account, botName string, subscribed, muted bool) error {
	if _, err := r.subscriptions.Raw().UpdateOne(ctx,
		bson.M{"u.account": account, "name": botName, "roomType": "botDM"},
		bson.M{"$set": bson.M{"isSubscribed": subscribed, "muted": muted}},
	); err != nil {
		return fmt.Errorf("update app subscription: %w", err)
	}
	return nil
}
