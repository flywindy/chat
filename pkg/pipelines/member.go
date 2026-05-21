// Package pipelines holds shared MongoDB aggregation pipelines used by more
// than one service. Putting them here lets each service append its own
// terminal stage (e.g. $count vs. $group) without duplicating the leading
// stages.
package pipelines

import (
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const botOrPseudoAccountRegex = `(\.bot$|^p_)`

// ErrRoomIDRequired is returned by pipeline builders that require a non-empty roomID.
var ErrRoomIDRequired = errors.New("roomID required")

// GetNewMembersPipeline returns the common stages for finding the unique,
// non-bot, not-already-subscribed users that an add-members request would
// add to roomID, given org IDs and direct account names.
//
// Pipeline target: the users collection.
//
// Stages: $match (org/account filter, exclude bots, optionally exclude one
// account), then (when roomID != "") $lookup + $match to filter out
// already-subscribed accounts. Empty roomID returns the $match stage only
// (used by capacity-check at create time, where the room doesn't exist yet
// so there are no subscriptions to filter against).
//
// excludeAccount is empty string to disable, or an account that must be
// dropped from the candidate set. Create-room callers pass the requester's
// account so the requester (who joins as owner separately) is not double-
// counted via org expansion.
//
// Callers MUST append a terminal stage that fits their need:
//   - room-service: bson.M{"$count": "n"}                                (capacity check)
//   - room-worker:  bson.M{"$group": {"_id": nil, "accounts": {"$addToSet": "$account"}}}
func GetNewMembersPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) bson.A {
	stages := bson.A{matchCandidates(orgIDs, directAccounts, excludeAccount)}
	if roomID != "" {
		stages = append(stages,
			bson.M{"$lookup": bson.M{
				"from": "subscriptions",
				"let":  bson.M{"userAccount": "$account"},
				"pipeline": bson.A{
					bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
						bson.M{"$eq": bson.A{"$roomId", roomID}},
						bson.M{"$eq": bson.A{"$u.account", "$$userAccount"}},
					}}}},
					bson.M{"$limit": 1},
					// Outer stage only checks emptiness — drop the full sub doc.
					bson.M{"$project": bson.M{"_id": 1}},
				},
				"as": "existingSub",
			}},
			bson.M{"$match": bson.M{"existingSub": bson.M{"$eq": bson.A{}}}},
		)
	}
	return stages
}

// matchCandidates: $match users by (sectId|deptId IN orgIDs) OR (account IN directAccounts), bot/excludeAccount filtered.
func matchCandidates(orgIDs, directAccounts []string, excludeAccount string) bson.M {
	orFilter := bson.A{}
	if len(orgIDs) > 0 {
		orFilter = append(orFilter, bson.M{"sectId": bson.M{"$in": orgIDs}}, bson.M{"deptId": bson.M{"$in": orgIDs}})
	}
	if len(directAccounts) > 0 {
		orFilter = append(orFilter, bson.M{"account": bson.M{"$in": directAccounts}})
	}
	accountFilter := bson.M{"$not": bson.Regex{Pattern: botOrPseudoAccountRegex, Options: ""}}
	if excludeAccount != "" {
		accountFilter["$ne"] = excludeAccount
	}
	return bson.M{"$match": bson.M{"$or": orFilter, "account": accountFilter}}
}

// GetAddMemberCandidatesPipeline returns per-candidate {account, hasSubscription, hasIndividualRoomMember} for the worker.
// Returns ErrRoomIDRequired if roomID is empty.
func GetAddMemberCandidatesPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) (bson.A, error) {
	if roomID == "" {
		return nil, ErrRoomIDRequired
	}
	return bson.A{
		matchCandidates(orgIDs, directAccounts, excludeAccount),
		bson.M{"$lookup": bson.M{
			"from": "subscriptions",
			"let":  bson.M{"acct": "$account"},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$roomId", roomID}},
					bson.M{"$eq": bson.A{"$u.account", "$$acct"}},
				}}}},
				bson.M{"$limit": 1},
				// Outer $project only reads $size of _sub — drop everything else.
				bson.M{"$project": bson.M{"_id": 1}},
			},
			"as": "_sub",
		}},
		bson.M{"$lookup": bson.M{
			"from": "room_members",
			"let":  bson.M{"uid": "$_id"},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$rid", roomID}},
					bson.M{"$eq": bson.A{"$member.type", "individual"}},
					bson.M{"$eq": bson.A{"$member.id", "$$uid"}},
				}}}},
				bson.M{"$limit": 1},
				// Same rationale as the _sub lookup above.
				bson.M{"$project": bson.M{"_id": 1}},
			},
			"as": "_irm",
		}},
		bson.M{"$project": bson.M{
			"_id":                     0,
			"account":                 "$account",
			"hasSubscription":         bson.M{"$gt": bson.A{bson.M{"$size": "$_sub"}, 0}},
			"hasIndividualRoomMember": bson.M{"$gt": bson.A{bson.M{"$size": "$_irm"}, 0}},
		}},
	}, nil
}
