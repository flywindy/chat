package mongorepo

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// accessSince gates to threads whose parent was created at or after the user's join time.
func buildBaseThreadMatch(roomID string, accessSince *time.Time) bson.M {
	match := bson.M{"roomId": roomID}
	if accessSince != nil {
		match["threadParentCreatedAt"] = bson.M{"$gte": *accessSince}
	}
	return match
}

func allThreadsPipeline(roomID string, accessSince *time.Time) bson.A {
	return bson.A{
		bson.D{{Key: "$match", Value: buildBaseThreadMatch(roomID, accessSince)}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}

func followingThreadsPipeline(roomID, account string, accessSince *time.Time) bson.A {
	match := buildBaseThreadMatch(roomID, accessSince)
	match["replyAccounts"] = account
	return bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}

// Unread = subscribed AND lastMsgAt > lastSeenAt (nil lastSeenAt = never seen = always unread).
func unreadThreadsPipeline(roomID, userAccount string, accessSince *time.Time) bson.A {
	match := buildBaseThreadMatch(roomID, accessSince)
	return bson.A{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from": "thread_subscriptions",
			"let":  bson.M{"tr": "$_id"},
			"pipeline": bson.A{
				bson.D{{Key: "$match", Value: bson.M{
					"$expr":       bson.M{"$eq": bson.A{"$threadRoomId", "$$tr"}},
					"userAccount": userAccount,
				}}},
				bson.D{{Key: "$project", Value: bson.M{"lastSeenAt": 1, "_id": 0}}},
			},
			"as": "sub",
		}}},
		// {$ne: []} — $lookup sets "sub" to [] when no subscription matched; non-empty means subscribed.
		bson.D{{Key: "$match", Value: bson.M{"sub": bson.M{"$ne": bson.A{}}}}},
		// null is the smallest BSON value, so $gt:[lastMsgAt, null] is true for any
		// non-null lastMsgAt — threads with nil lastSeenAt (never seen) are included.
		bson.D{{Key: "$match", Value: bson.M{
			"$expr": bson.M{"$gt": bson.A{"$lastMsgAt", bson.M{"$arrayElemAt": bson.A{"$sub.lastSeenAt", 0}}}},
		}}},
		bson.D{{Key: "$project", Value: bson.M{"sub": 0}}},
		bson.D{{Key: "$sort", Value: threadRoomSort}},
	}
}
