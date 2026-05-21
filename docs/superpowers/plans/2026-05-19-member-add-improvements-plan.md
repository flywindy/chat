# Member-Add Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the three changes specified in `docs/superpowers/specs/2026-05-19-org-to-individual-membership-upgrade-design.md`: (Part 1) fix the silent no-op when an already-org-subscribed user is added individually; (Part 2) accept `deptId` values inside the `orgs` field with prefer-dept-on-overlap display; (Part 3) frontend dedup-reply navigates directly without entering the wait-for-summaries state.

**Architecture:** Backend pipeline + worker changes in Go (`pkg/pipelines`, `room-worker`, `room-service`); frontend change in React (`chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog`). All new aggregations index-covered via existing or one new MongoDB index `(deptId, account)` on `users`. TDD throughout: failing test → implementation → green → commit per task.

**Tech Stack:** Go 1.25, MongoDB driver v2, mockgen for unit mocks, testcontainers-go for integration tests, Gin (room-service HTTP), nats.go + JetStream (workers). Frontend: React + vitest + @testing-library/react.

---

## File map

**Modify:**
- `pkg/pipelines/member.go` — add `GetCapacityCheckPipeline` + `GetAddMemberCandidatesPipeline`, extend $or to match `deptId`.
- `pkg/model/user.go` — add `SectTCName`, `DeptID`, `DeptName`, `DeptTCName` fields.
- `pkg/model/model_test.go` — roundtrip test covers new fields.
- `room-worker/store.go` — add `AddMemberCandidate` type + `ListAddMemberCandidates` method; extend `OrgMemberStatus`; remove `ListNewMembers`.
- `room-worker/store_mongo.go` — implementations; update `GetOrgMembersWithIndividualStatus` to use `member.id` lookup + new fields.
- `room-worker/handler.go` — `processAddMembers` rewrite around new method; `processRemoveOrg` two-pass dept-first tiebreak with `displayOrg` formatting; drop "missing SectName" permanent error.
- `room-worker/handler_test.go` — table-driven cases for the new branches.
- `room-worker/integration_test.go` — integration cases for the org→individual upgrade and dept-matched org-remove.
- `room-worker/sysmsg.go` — extract `combineWithFallback`, refactor `displayName`, add `displayOrg`, update `formatRemovedOrg` signature.
- `room-worker/sysmsg_test.go` — new test file for the helpers.
- `room-worker/mock_store_test.go` — regenerated.
- `room-service/store_mongo.go` — swap `CountNewMembers` to `GetCapacityCheckPipeline`; add `(deptId, account)` index in `EnsureIndexes`; update enrichment `_orgMatch` for prefer-dept + orgId fallback.
- `room-service/integration_test.go` — capacity-regression + enrichment-dept cases.
- `chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.jsx` — short-circuit dedup branch.
- `chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.test.jsx` — rewrite the existing dedup test + new race test.

**No change:**
- `pkg/model/member.go` — `MemberRemoved.SectName` field name stays; semantics broaden in worker.
- `room-service/handler.go`, `room-service/store.go` — public surface unchanged.
- `room_members` collection schema unchanged; `member.type` stays `"org"`.

---

# Part 1 — Backend bug fix: org→individual silent no-op

## Task 1: Add `GetCapacityCheckPipeline` and `GetAddMemberCandidatesPipeline`

**Files:**
- Modify: `pkg/pipelines/member.go`

**Approach:** Add two new pipeline builders next to the existing `GetNewMembersPipeline`. Share a private `matchCandidates` helper for the common `$match` stage. No tests in this task — the pipelines are data builders, verified via the integration test in Task 2.

- [ ] **Step 1: Add the helper + new pipeline functions to `pkg/pipelines/member.go`**

Append after the existing `GetNewMembersPipeline`:

```go
// matchCandidates: $match users by (sectId|deptId IN orgIDs) OR (account IN directAccounts), bot/excludeAccount filtered.
func matchCandidates(orgIDs, directAccounts []string, excludeAccount string) bson.M {
	orFilter := bson.A{}
	if len(orgIDs) > 0 {
		orFilter = append(orFilter, bson.M{"sectId": bson.M{"$in": orgIDs}})
	}
	if len(directAccounts) > 0 {
		orFilter = append(orFilter, bson.M{"account": bson.M{"$in": directAccounts}})
	}
	accountFilter := bson.M{"$not": bson.Regex{Pattern: `(\.bot$|^p_)`, Options: ""}}
	if excludeAccount != "" {
		accountFilter["$ne"] = excludeAccount
	}
	return bson.M{"$match": bson.M{"$or": orFilter, "account": accountFilter}}
}

// GetCapacityCheckPipeline counts net-new subscriptions for (orgIDs, directAccounts) in roomID; caller appends $count.
func GetCapacityCheckPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) bson.A {
	if roomID == "" {
		panic("GetCapacityCheckPipeline: roomID required")
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
			},
			"as": "_sub",
		}},
		bson.M{"$match": bson.M{"_sub": bson.M{"$eq": bson.A{}}}},
	}
}

// GetAddMemberCandidatesPipeline returns per-candidate {account, hasSubscription, hasIndividualRoomMember} for the worker.
func GetAddMemberCandidatesPipeline(orgIDs, directAccounts []string, roomID, excludeAccount string) bson.A {
	if roomID == "" {
		panic("GetAddMemberCandidatesPipeline: roomID required")
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
			},
			"as": "_irm",
		}},
		bson.M{"$project": bson.M{
			"_id":                     0,
			"account":                 "$account",
			"hasSubscription":         bson.M{"$gt": bson.A{bson.M{"$size": "$_sub"}, 0}},
			"hasIndividualRoomMember": bson.M{"$gt": bson.A{bson.M{"$size": "$_irm"}, 0}},
		}},
	}
}
```

- [ ] **Step 2: Verify compilation**

```sh
make build SERVICE=room-worker
make build SERVICE=room-service
```

Both should succeed. No tests are run yet — they fail at the next task.

- [ ] **Step 3: Commit**

```sh
git add pkg/pipelines/member.go
git commit -m "feat(pipelines): add GetCapacityCheckPipeline + GetAddMemberCandidatesPipeline"
```

---

## Task 2: Add `AddMemberCandidate` type + `ListAddMemberCandidates` store method (room-worker)

**Files:**
- Modify: `room-worker/store.go`, `room-worker/store_mongo.go`
- Test: `room-worker/integration_test.go`
- Regenerate: `room-worker/mock_store_test.go`

**Approach:** Add the new method to the store interface, write an integration test against testcontainers MongoDB that verifies the per-flag output for the four key states (truly new, has-sub-no-IRM, no-sub-has-IRM impossible state, has-sub-has-IRM), implement the method backed by `GetAddMemberCandidatesPipeline`.

- [ ] **Step 1: Add the `AddMemberCandidate` type and interface method to `room-worker/store.go`**

Insert after `OrgMemberStatus` (around line 33):

```go
// AddMemberCandidate is one element returned by ListAddMemberCandidates.
type AddMemberCandidate struct {
	Account                 string `bson:"account"`
	HasSubscription         bool   `bson:"hasSubscription"`
	HasIndividualRoomMember bool   `bson:"hasIndividualRoomMember"`
}
```

Add to the `SubscriptionStore` interface (after `ListNewMembers` on line 74):

```go
	// ListAddMemberCandidates: per-user {hasSub, hasIndividualRow} flags so the worker splits into needSub vs needIRM (org→individual upgrade).
	ListAddMemberCandidates(ctx context.Context, orgIDs, directAccounts []string, roomID string) ([]AddMemberCandidate, error)
```

- [ ] **Step 2: Regenerate mocks**

```sh
make generate SERVICE=room-worker
```

Expect: `room-worker/mock_store_test.go` updated; no unrelated diff. Verify with `git diff --stat room-worker/mock_store_test.go`.

- [ ] **Step 3: Write the failing integration test**

Add to `room-worker/integration_test.go` (find the section near other `Test*MongoStore_*` integration tests; preserve `//go:build integration` placement):

```go
func TestMongoStore_ListAddMemberCandidates_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)

	// Seed: alice (new), bob (sub only — bug scenario), carol (sub+IRM), dave (bot, excluded).
	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice", SectID: "org-eng", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob", SectID: "org-eng", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_carol", Account: "carol", SectID: "org-eng", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_dave", Account: "dave.bot", SectID: "org-eng", SiteID: "site-a"})

	const roomID = "room-1"
	// bob: subscription only (the bug scenario — added via org earlier).
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_bob", Account: "bob"},
		RoomType: model.RoomTypeChannel, Roles: []model.Role{model.RoleMember},
	})
	// carol: subscription + individual room_member.
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_carol", Account: "carol"},
		RoomType: model.RoomTypeChannel, Roles: []model.Role{model.RoleMember},
	})
	_, err := db.Collection("room_members").InsertOne(ctx, model.RoomMember{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID,
		Member: model.RoomMemberEntry{ID: "u_carol", Type: model.RoomMemberIndividual, Account: "carol"},
	})
	require.NoError(t, err)

	got, err := store.ListAddMemberCandidates(ctx, []string{"org-eng"}, nil, roomID)
	require.NoError(t, err)

	byAccount := map[string]AddMemberCandidate{}
	for _, c := range got {
		byAccount[c.Account] = c
	}
	require.Len(t, byAccount, 3, "bot dave.bot must be excluded")
	assert.Equal(t, AddMemberCandidate{Account: "alice", HasSubscription: false, HasIndividualRoomMember: false}, byAccount["alice"])
	assert.Equal(t, AddMemberCandidate{Account: "bob", HasSubscription: true, HasIndividualRoomMember: false}, byAccount["bob"], "bug scenario: sub exists, IRM does not")
	assert.Equal(t, AddMemberCandidate{Account: "carol", HasSubscription: true, HasIndividualRoomMember: true}, byAccount["carol"])
}
```

Note: the existing file uses inline `db.Collection("room_members").InsertOne(ctx, ...)` for room_members inserts. Replace the `mustInsertRoomMember(...)` call above with that pattern (or add a small `mustInsertRoomMember(t, db, rm)` helper next to `mustInsertSub` at line ~422 modeled on `mustInsertSub`).

- [ ] **Step 4: Run the test and verify it fails with "method not defined"**

```sh
make test-integration SERVICE=room-worker
```

Expected: compile error or panic from missing `ListAddMemberCandidates` on `MongoStore`.

- [ ] **Step 5: Implement `ListAddMemberCandidates` in `room-worker/store_mongo.go`**

Insert next to the existing `ListNewMembers` (around line 387):

```go
func (s *MongoStore) ListAddMemberCandidates(ctx context.Context, orgIDs, directAccounts []string, roomID string) ([]AddMemberCandidate, error) {
	if len(orgIDs) == 0 && len(directAccounts) == 0 {
		return nil, nil
	}
	pipeline := pipelines.GetAddMemberCandidatesPipeline(orgIDs, directAccounts, roomID, "")
	cursor, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregate add-member candidates: %w", err)
	}
	defer cursor.Close(ctx)
	var out []AddMemberCandidate
	if err := cursor.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("decode add-member candidates: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 6: Run the integration test, verify it passes**

```
make test-integration SERVICE=room-worker
```

Expected: PASS.

- [ ] **Step 7: Run unit tests to confirm no regressions**

```
make test SERVICE=room-worker
```

Expected: PASS (mocks regenerated; nothing in `handler_test.go` references the new method yet).

- [ ] **Step 8: Commit**

```
git add room-worker/store.go room-worker/store_mongo.go room-worker/integration_test.go room-worker/mock_store_test.go
git commit -m "feat(room-worker): add ListAddMemberCandidates with per-candidate flags"
```

---

## Task 3: Rewrite `processAddMembers` in `room-worker/handler.go`

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`
- Existing: `room-worker/integration_test.go`

**Approach:** Replace the `ListNewMembers` call with `ListAddMemberCandidates`. Split candidates into `needSub` and `needIndividualRoomMember`. Early-return only when both sets are empty AND `req.Orgs` is empty. The backfill loop's "already processed" set becomes `needSub ∪ needIndividualRoomMember`. Existing happy-path tests must continue to pass; add the bug-scenario test first.

- [ ] **Step 1: Add the failing handler test for the org→individual upgrade**

Add to `room-worker/handler_test.go` (find existing `TestHandler_ProcessAddMembers_*` cases as the model):

```go
func TestHandler_ProcessAddMembers_OrgToIndividualUpgrade(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockSubscriptionStore(ctrl)
	keyStore := NewMockRoomKeyStore(ctrl)
	h := newTestHandler(store, keyStore, "site-a")

	roomID := "room-1"
	requestID := idgen.GenerateRequestID()
	ctx := natsutil.WithRequestID(context.Background(), requestID)

	store.EXPECT().GetRoom(ctx, roomID).Return(&model.Room{
		ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a", Name: "Room 1",
	}, nil)
	// Alice has a subscription (added earlier via org) but no individual row.
	store.EXPECT().ListAddMemberCandidates(ctx, []string{}, []string{"alice"}, roomID).Return([]AddMemberCandidate{
		{Account: "alice", HasSubscription: true, HasIndividualRoomMember: false},
	}, nil)
	store.EXPECT().FindUsersByAccounts(ctx, []string{"alice"}).Return([]model.User{
		{ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝", SiteID: "site-a"},
	}, nil)
	store.EXPECT().GetUser(ctx, "owner").Return(&model.User{
		ID: "u_owner", Account: "owner", EngName: "Owner", ChineseName: "拥有者", SiteID: "site-a",
	}, nil)
	// No BulkCreateSubscriptions (alice already subscribed); BulkCreateRoomMembers with one individual row.
	store.EXPECT().HasOrgRoomMembers(ctx, roomID).Return(true, nil)
	store.EXPECT().BulkCreateRoomMembers(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, members []*model.RoomMember) error {
		require.Len(t, members, 1)
		assert.Equal(t, model.RoomMemberIndividual, members[0].Member.Type)
		assert.Equal(t, "alice", members[0].Member.Account)
		return nil
	})
	store.EXPECT().ReconcileMemberCounts(ctx, roomID).Return(nil)
	publishMock := newPublishRecorder(t, h)

	req := model.AddMembersRequest{
		RoomID: roomID, Users: []string{"alice"}, RequesterAccount: "owner", RequesterID: "u_owner",
		Timestamp: time.Now().UTC().UnixMilli(),
	}
	data, _ := json.Marshal(req)
	err := h.processAddMembers(ctx, data)
	require.NoError(t, err)

	// Confirm the upgrade path still emits its events: a subscription.update for alice and the canonical sys-message for "added".
	publishMock.AssertSubjectFired(t, subject.SubscriptionUpdate("alice"))
	publishMock.AssertSubjectFired(t, subject.MsgCanonicalCreated("site-a"))
}
```

`newPublishRecorder` returns a recorder with the same `publish` signature used by the handler, exposing helpers like `AssertSubjectFired(t, subject)` to verify publishes. Model on the existing publish capture pattern at `room-worker/integration_test.go:626, 700` if the recorder helper doesn't exist yet — add it next to that pattern.

- [ ] **Step 2: Run the test, verify it fails**

```
make test SERVICE=room-worker -run TestHandler_ProcessAddMembers_OrgToIndividualUpgrade
```

Expected: FAIL — `ListAddMemberCandidates` is mocked but `processAddMembers` still calls `ListNewMembers`.

- [ ] **Step 3: Rewrite `processAddMembers` in `room-worker/handler.go`**

Replace the block between line 710 (`accounts, err := h.store.ListNewMembers(...)`) and line 832 (end of the org `room_members` loop) with:

```go
	// Resolve candidates with per-flag membership status.
	candidates, err := h.store.ListAddMemberCandidates(ctx, req.Orgs, req.Users, req.RoomID)
	if err != nil {
		return fmt.Errorf("list add-member candidates: %w", err)
	}

	// Fail closed: defaulting hadOrgsBefore=false on error would trigger spurious first-org backfill.
	hadOrgsBefore, err := h.store.HasOrgRoomMembers(ctx, req.RoomID)
	if err != nil {
		return fmt.Errorf("check existing org room members: %w", err)
	}
	writeIndividuals := len(req.Orgs) > 0 || hadOrgsBefore

	allowedIndividual := make(map[string]struct{}, len(req.Users))
	for _, acc := range req.Users {
		allowedIndividual[acc] = struct{}{}
	}

	// needSub = no sub yet; needIRM = no individual row yet (writeIndividuals-gated, req.Users only).
	var needSub []AddMemberCandidate
	var needIRM []AddMemberCandidate
	for _, c := range candidates {
		if !c.HasSubscription {
			needSub = append(needSub, c)
		}
		if writeIndividuals && !c.HasIndividualRoomMember {
			if _, ok := allowedIndividual[c.Account]; ok {
				needIRM = append(needIRM, c)
			}
		}
	}

	// Nothing to write: no new subs, no individual upgrades, no org rows.
	if len(needSub) == 0 && len(needIRM) == 0 && len(req.Orgs) == 0 {
		return nil
	}

	// Build the lookup-account set: anyone whose sub or individual row we'll write.
	lookupAccounts := make([]string, 0, len(needSub)+len(needIRM))
	seen := make(map[string]struct{}, len(needSub)+len(needIRM))
	for _, c := range needSub {
		if _, ok := seen[c.Account]; !ok {
			lookupAccounts = append(lookupAccounts, c.Account)
			seen[c.Account] = struct{}{}
		}
	}
	for _, c := range needIRM {
		if _, ok := seen[c.Account]; !ok {
			lookupAccounts = append(lookupAccounts, c.Account)
			seen[c.Account] = struct{}{}
		}
	}

	var userMap map[string]model.User
	if len(lookupAccounts) > 0 {
		users, err := h.store.FindUsersByAccounts(ctx, lookupAccounts)
		if err != nil {
			return fmt.Errorf("find users by accounts: %w", err)
		}
		userMap = make(map[string]model.User, len(users))
		for i := range users {
			userMap[users[i].Account] = users[i]
		}
		for _, acc := range lookupAccounts {
			if _, ok := userMap[acc]; !ok {
				return newPermanent("user %s not found in room.member.add (room %s)", acc, req.RoomID)
			}
		}
	}

	requester, err := h.store.GetUser(ctx, req.RequesterAccount)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return newPermanent("requester %s not found (room %s)", req.RequesterAccount, req.RoomID)
		}
		return fmt.Errorf("get requester: %w", err)
	}

	acceptedAt := time.UnixMilli(req.Timestamp).UTC()
	now := time.Now().UTC()

	// Build subs only for needSub.
	subs := make([]*model.Subscription, 0, len(needSub))
	actualAccounts := make([]string, 0, len(needSub))
	for _, c := range needSub {
		user := userMap[c.Account]
		sub := &model.Subscription{
			ID:       idgen.GenerateUUIDv7(),
			User:     model.SubscriptionUser{ID: user.ID, Account: user.Account},
			RoomID:   req.RoomID,
			Name:     room.Name,
			RoomType: model.RoomTypeChannel,
			SiteID:   room.SiteID,
			Roles:    []model.Role{model.RoleMember},
			JoinedAt: acceptedAt,
		}
		if ms := historySharedSincePtr(req.History, req.Timestamp, req.RoomID); ms != nil {
			t := time.UnixMilli(*ms).UTC()
			sub.HistorySharedSince = &t
		}
		subs = append(subs, sub)
		actualAccounts = append(actualAccounts, user.Account)
	}

	if len(subs) > 0 {
		if err := h.store.BulkCreateSubscriptions(ctx, subs); err != nil {
			return fmt.Errorf("bulk create subscriptions: %w", err)
		}
	}

	// Build room_members: individuals (needIRM) + orgs (req.Orgs).
	roomMembers := make([]*model.RoomMember, 0, len(needIRM)+len(req.Orgs))
	processedAccounts := make(map[string]struct{}, len(needSub)+len(needIRM))
	for _, c := range needSub {
		processedAccounts[c.Account] = struct{}{}
	}
	for _, c := range needIRM {
		processedAccounts[c.Account] = struct{}{}
		user := userMap[c.Account]
		roomMembers = append(roomMembers, &model.RoomMember{
			ID:     idgen.GenerateUUIDv7(),
			RoomID: req.RoomID,
			Ts:     acceptedAt,
			Member: model.RoomMemberEntry{
				ID:      user.ID,
				Type:    model.RoomMemberIndividual,
				Account: user.Account,
			},
		})
	}
	for _, org := range req.Orgs {
		roomMembers = append(roomMembers, &model.RoomMember{
			ID:     idgen.GenerateUUIDv7(),
			RoomID: req.RoomID,
			Ts:     acceptedAt,
			Member: model.RoomMemberEntry{ID: org, Type: model.RoomMemberOrg},
		})
	}
```

Then update the backfill block (currently at lines 836-867) to use `processedAccounts` instead of `resolvedAccountSet`:

```go
	if len(req.Orgs) > 0 && !hadOrgsBefore {
		existingAccounts, err := h.store.GetSubscriptionAccounts(ctx, req.RoomID)
		if err != nil {
			slog.Warn("get subscription accounts for backfill failed", "error", err)
		} else {
			var backfillAccounts []string
			for _, account := range existingAccounts {
				if _, processed := processedAccounts[account]; !processed {
					backfillAccounts = append(backfillAccounts, account)
				}
			}
			if len(backfillAccounts) > 0 {
				backfillUsers, err := h.store.FindUsersByAccounts(ctx, backfillAccounts)
				if err != nil {
					slog.Warn("find users for backfill failed", "error", err)
				} else {
					for i := range backfillUsers {
						roomMembers = append(roomMembers, &model.RoomMember{
							ID:     idgen.GenerateUUIDv7(),
							RoomID: req.RoomID,
							Ts:     acceptedAt,
							Member: model.RoomMemberEntry{
								ID:      backfillUsers[i].ID,
								Type:    model.RoomMemberIndividual,
								Account: backfillUsers[i].Account,
							},
						})
					}
				}
			}
		}
	}
```

The rest of the function (`BulkCreateRoomMembers`, `ReconcileMemberCounts`, event publish, sys-msg publish) is unchanged.

Also remove the unused `resolvedAccountSet` and `resolvedAccountSet[user.Account] = struct{}{}` line if you spot it in the deleted block.

- [ ] **Step 4: Run the new test and verify it passes**

```
make test SERVICE=room-worker -run TestHandler_ProcessAddMembers_OrgToIndividualUpgrade
```

Expected: PASS.

- [ ] **Step 5: Run all room-worker unit tests for regressions**

```
make test SERVICE=room-worker
```

Expected: PASS for all existing `TestHandler_ProcessAddMembers_*` cases (they used `ListNewMembers`; now they need to use `ListAddMemberCandidates`). Update each existing test's `store.EXPECT().ListNewMembers(...)` call to `store.EXPECT().ListAddMemberCandidates(...)` with equivalent semantics:
- Where the test expected `ListNewMembers` to return `["alice"]` (truly new), now expect `ListAddMemberCandidates` to return `[{Account: "alice", HasSubscription: false, HasIndividualRoomMember: false}]`.
- Where the test expected `ListNewMembers` to return `[]` (everyone already subscribed), now expect `ListAddMemberCandidates` to return per-existing-user candidate rows with `HasSubscription: true`.

Re-run after fixups.

- [ ] **Step 6: Remove `ListNewMembers` from the store interface and implementation**

Edit `room-worker/store.go` to remove the `ListNewMembers` method declaration (lines 68-74) and the comment block above it.

Edit `room-worker/store_mongo.go` to remove the `ListNewMembers` function body (lines 387+).

Regenerate mocks:

```
make generate SERVICE=room-worker
```

Build and run tests:

```
make test SERVICE=room-worker
make test-integration SERVICE=room-worker
```

Both pass.

- [ ] **Step 7: Add an integration test for the bug scenario**

Add to `room-worker/integration_test.go`:

```go
func TestHandler_ProcessAddMembers_OrgToIndividualUpgrade_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)
	cap := newPublishCapture(t)
	h := NewHandler(store, "site-a", cap.fn(), testKeyStore, testKeySender)

	const roomID = "room-1"
	mustInsertRoom(t, db, &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a", Name: "Room 1"})
	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱丽丝", SectID: "org-eng", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_owner", Account: "owner", EngName: "Owner", ChineseName: "拥有者", SiteID: "site-a"})
	// Pre-state: alice is in the room via org-eng. Subscription exists, org room_members row exists, no individual row.
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_alice", Account: "alice"},
		RoomType: model.RoomTypeChannel, Roles: []model.Role{model.RoleMember},
	})
	_, err := db.Collection("room_members").InsertOne(ctx, model.RoomMember{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID,
		Member: model.RoomMemberEntry{ID: "org-eng", Type: model.RoomMemberOrg},
	})
	require.NoError(t, err)

	req := model.AddMembersRequest{
		RoomID: roomID, Users: []string{"alice"}, RequesterAccount: "owner", RequesterID: "u_owner",
		Timestamp: time.Now().UTC().UnixMilli(),
	}
	data, _ := json.Marshal(req)
	requestID := idgen.GenerateRequestID()
	require.NoError(t, h.processAddMembers(natsutil.WithRequestID(ctx, requestID), data))

	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": roomID, "u.account": "alice"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), subCount, "no duplicate subscription created")

	indivCount, err := db.Collection("room_members").CountDocuments(ctx, bson.M{
		"rid": roomID, "member.type": "individual", "member.account": "alice",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), indivCount, "individual room_members row written on the upgrade path")
}
```

`newPublishCapture`, `testKeyStore`, `testKeySender` are existing helpers used elsewhere in the file (see lines around 626, 700 for the pattern).

- [ ] **Step 8: Run integration tests**

```
make test-integration SERVICE=room-worker
```

Expected: PASS.

- [ ] **Step 9: Commit**

```
git add room-worker/store.go room-worker/store_mongo.go room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go room-worker/mock_store_test.go
git commit -m "fix(room-worker): create individual room_members row on org→individual member.add"
```

---

## Task 4: Swap `CountNewMembers` in `room-service/store_mongo.go` to the lite pipeline

**Files:**
- Modify: `room-service/store_mongo.go`, `room-service/integration_test.go`

**Approach:** Replace `GetNewMembersPipeline` with `GetCapacityCheckPipeline` for the `roomID != ""` path. Add a regression test that confirms re-adding an org-only user as an individual returns 0 (capacity unchanged).

- [ ] **Step 1: Add the failing capacity-regression test**

Add to `room-service/integration_test.go`:

```go
func TestMongoStore_CountNewMembers_OrgOnlyUserCountsZero_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)
	require.NoError(t, store.EnsureIndexes(ctx))

	const roomID = "room-1"
	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice", SectID: "org-eng", SiteID: "site-a"})
	// Alice already has a subscription via org-eng — adding her individually should add 0 new subs.
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_alice", Account: "alice"},
		RoomType: model.RoomTypeChannel,
	})

	n, err := store.CountNewMembers(ctx, nil, []string{"alice"}, roomID, "")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "alice already has a sub via org — capacity unchanged")
}
```

- [ ] **Step 2: Run, verify it currently passes (the existing pipeline already filters by subscription)**

```
make test-integration SERVICE=room-service -run TestMongoStore_CountNewMembers_OrgOnlyUserCountsZero_Integration
```

Expected: PASS (today the existing `GetNewMembersPipeline` happens to filter subscribed users; the test guards against regression while we swap the pipeline).

- [ ] **Step 3: Swap the pipeline in `room-service/store_mongo.go` `CountNewMembers`**

Locate `CountNewMembers` (around line 278). For the `roomID != ""` path, switch from `pipelines.GetNewMembersPipeline(orgIDs, directAccounts, roomID, excludeAccount)` to `pipelines.GetCapacityCheckPipeline(orgIDs, directAccounts, roomID, excludeAccount)`. Keep `GetNewMembersPipeline` for the `roomID == ""` create-room case. Full diff:

```go
func (s *MongoStore) CountNewMembers(ctx context.Context, orgIDs, directAccounts []string, roomID, excludeAccount string) (int, error) {
	if len(orgIDs) == 0 && len(directAccounts) == 0 {
		return 0, nil
	}
	var pipeline bson.A
	if roomID == "" {
		pipeline = pipelines.GetNewMembersPipeline(orgIDs, directAccounts, "", excludeAccount)
	} else {
		pipeline = pipelines.GetCapacityCheckPipeline(orgIDs, directAccounts, roomID, excludeAccount)
	}
	pipeline = append(pipeline, bson.M{"$count": "n"})

	cursor, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("count new members: %w", err)
	}
	var results []struct {
		Count int `bson:"n"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return 0, fmt.Errorf("decode count new members: %w", err)
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0].Count, nil
}
```

- [ ] **Step 4: Run tests**

```
make test-integration SERVICE=room-service
make test SERVICE=room-service
```

Both pass.

- [ ] **Step 5: Commit**

```
git add room-service/store_mongo.go room-service/integration_test.go
git commit -m "perf(room-service): use GetCapacityCheckPipeline for CountNewMembers"
```

---

# Part 2 — DeptId feature

## Task 5: Add `SectTCName`, `DeptID`, `DeptName`, `DeptTCName` fields to `User`

**Files:**
- Modify: `pkg/model/user.go`, `pkg/model/model_test.go`

- [ ] **Step 1: Extend the failing roundtrip test for `User` in `pkg/model/model_test.go`**

Find the existing `User` roundtrip case (likely in a table-driven test like `TestRoundTripModels` that iterates over types). Add field values for the new fields:

```go
{
	name: "User with sect+dept",
	in: &User{
		ID: "u1", Account: "alice", SiteID: "site-a",
		SectID: "S", SectName: "Sect", SectTCName: "部",
		DeptID: "D", DeptName: "Dept", DeptTCName: "處",
		EngName: "Alice", ChineseName: "爱丽丝",
	},
},
```

If the file uses a generic `roundTrip` helper, no new test function is needed — augment the existing data table.

- [ ] **Step 2: Run the test, verify it fails**

```
make test SERVICE=pkg/model
```

(If `make test SERVICE=` only accepts top-level service names, run `cd pkg/model && go test ./...` instead.)

Expected: FAIL — `SectTCName`/`DeptID`/`DeptName`/`DeptTCName` are not fields on `User`.

- [ ] **Step 3: Add the fields to `pkg/model/user.go`**

```go
type User struct {
	ID          string `json:"id"           bson:"_id"`
	Account     string `json:"account"      bson:"account"`
	SiteID      string `json:"siteId"       bson:"siteId"`
	SectID      string `json:"sectId"       bson:"sectId"`
	SectName    string `json:"sectName"     bson:"sectName"`
	SectTCName  string `json:"sectTCName"   bson:"sectTCName"`
	DeptID      string `json:"deptId"       bson:"deptId"`
	DeptName    string `json:"deptName"     bson:"deptName"`
	DeptTCName  string `json:"deptTCName"   bson:"deptTCName"`
	EngName     string `json:"engName"      bson:"engName"`
	ChineseName string `json:"chineseName"  bson:"chineseName"`
	EmployeeID  string `json:"employeeId"   bson:"employeeId"`
}
```

- [ ] **Step 4: Run tests**

```
make test SERVICE=pkg/model
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add pkg/model/user.go pkg/model/model_test.go
git commit -m "feat(model): add SectTCName, DeptID, DeptName, DeptTCName fields to User"
```

---

## Task 6: Add `(deptId, account)` index in `room-service/store_mongo.go EnsureIndexes`

**Files:**
- Modify: `room-service/store_mongo.go`

**Approach:** Mirror the existing `(sectId, account)` index. Idempotent index creation; no test needed beyond confirming `EnsureIndexes` still returns nil for an existing setup.

- [ ] **Step 1: Add the index creation**

In `room-service/store_mongo.go` `EnsureIndexes`, after the existing `(sectId, account)` index block (around line 68-72), append:

```go
	if _, err := s.users.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "deptId", Value: 1}, {Key: "account", Value: 1}},
	}); err != nil {
		return fmt.Errorf("ensure users (deptId,account) index: %w", err)
	}
```

- [ ] **Step 2: Run the existing integration tests; `EnsureIndexes` is called by the test setup**

```
make test-integration SERVICE=room-service
```

Expected: PASS.

- [ ] **Step 3: Commit**

```
git add room-service/store_mongo.go
git commit -m "feat(room-service): add (deptId, account) index for deptId-matching pipelines"
```

---

## Task 7: Extract `combineWithFallback`, refactor `displayName`, add `displayOrg`, update `formatRemovedOrg`

**Files:**
- Modify: `room-worker/sysmsg.go`
- Create: `room-worker/sysmsg_test.go` (if not present)

**Approach:** Refactor `displayName(user)` to call a shared helper, add the parallel `displayOrg(name, tcName, orgID)`, change `formatRemovedOrg` signature from `(sectName)` to `(name, tcName, orgID)`. The change is callable but unused until Task 10.

- [ ] **Step 1: Write failing unit tests in `room-worker/sysmsg_test.go`**

Create the file if it doesn't exist, mirroring the package-internal test style:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hmchangw/chat/pkg/model"
)

func TestCombineWithFallback(t *testing.T) {
	tests := []struct {
		name              string
		first, second, fb string
		want              string
	}{
		{"both", "Eng", "中", "x", "Eng 中"},
		{"only first", "Eng", "", "x", "Eng"},
		{"only second", "", "中", "x", "中"},
		{"both empty", "", "", "fallback", "fallback"},
		{"equal halves", "Same", "Same", "x", "Same"},
		{"first whitespace", "   ", "中", "x", "中"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, combineWithFallback(tc.first, tc.second, tc.fb))
		})
	}
}

func TestDisplayName_DelegatesToCombineWithFallback(t *testing.T) {
	u := &model.User{Account: "alice", EngName: "Alice", ChineseName: "爱丽丝"}
	assert.Equal(t, "Alice 爱丽丝", displayName(u))

	u2 := &model.User{Account: "bob"}
	assert.Equal(t, "bob", displayName(u2), "both names empty → falls back to Account")
}

func TestDisplayOrg(t *testing.T) {
	assert.Equal(t, "Eng 工程部", displayOrg("Eng", "工程部", "orgX"))
	assert.Equal(t, "Eng", displayOrg("Eng", "", "orgX"))
	assert.Equal(t, "工程部", displayOrg("", "工程部", "orgX"))
	assert.Equal(t, "orgX", displayOrg("", "", "orgX"))
}

func TestFormatRemovedOrg(t *testing.T) {
	assert.Equal(t, `"Eng 工程部" has been removed from the channel`, formatRemovedOrg("Eng", "工程部", "orgX"))
	assert.Equal(t, `"orgX" has been removed from the channel`, formatRemovedOrg("", "", "orgX"))
}
```

- [ ] **Step 2: Run, verify it fails**

```
make test SERVICE=room-worker -run "TestCombineWithFallback|TestDisplayOrg|TestFormatRemovedOrg"
```

Expected: FAIL — `combineWithFallback` and `displayOrg` undefined; `formatRemovedOrg` has the wrong signature.

- [ ] **Step 3: Create the shared helper in `pkg/displayfmt/combine.go`**

Both room-worker (sys-messages) and room-service (member-list enrichment) need the same combine logic. Put it in a shared package so there's exactly one definition:

```go
// pkg/displayfmt/combine.go
package displayfmt

import "strings"

// CombineWithFallback returns first+" "+second when both present, the non-empty side, or fallback when both empty.
func CombineWithFallback(first, second, fallback string) string {
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "" && second == "":
		return fallback
	case first == "":
		return second
	case second == "":
		return first
	case first == second:
		return first
	default:
		return first + " " + second
	}
}
```

Also add `pkg/displayfmt/combine_test.go` with the same table-driven cases as Step 1 above (rewrite as `TestCombineWithFallback` against the exported `displayfmt.CombineWithFallback`). The Step-1 worker-only test is then trimmed to cover only `displayName`/`displayOrg`/`formatRemovedOrg`.

- [ ] **Step 4: Update `room-worker/sysmsg.go` to delegate**

Replace the file's current contents below the package declaration with:

```go
package main

import (
	"github.com/hmchangw/chat/pkg/displayfmt"
	"github.com/hmchangw/chat/pkg/model"
)

func displayName(u *model.User) string {
	return displayfmt.CombineWithFallback(u.EngName, u.ChineseName, u.Account)
}

func displayOrg(name, tcName, orgID string) string {
	return displayfmt.CombineWithFallback(name, tcName, orgID)
}

func quoted(name string) string {
	return "\"" + name + "\""
}

func formatAddedSingle(requester, added *model.User) string {
	return quoted(displayName(requester)) + " added " + quoted(displayName(added)) + " to the channel"
}

func formatAddedMulti(requester *model.User) string {
	return quoted(displayName(requester)) + " added members to the channel"
}

func formatRemovedUser(user *model.User) string {
	return quoted(displayName(user)) + " has been removed from the channel"
}

func formatRemovedOrg(name, tcName, orgID string) string {
	return quoted(displayOrg(name, tcName, orgID)) + " has been removed from the channel"
}

func formatLeft(user *model.User) string {
	return quoted(displayName(user)) + " left the channel"
}
```

- [ ] **Step 5: Update the one current caller of `formatRemovedOrg` in `room-worker/handler.go` to compile**

In `processRemoveOrg` (around line 631), update the temporary call site so the build is green; Task 10 will rewrite the surrounding logic:

```go
Content: formatRemovedOrg(sectName, "", req.OrgID),
```

This is a transitional stub — Task 10 replaces it with the proper dept-first resolution.

- [ ] **Step 6: Run the unit tests**

```
make test SERVICE=pkg/displayfmt
make test SERVICE=room-worker
```

Both pass.

- [ ] **Step 7: Commit**

```
git add pkg/displayfmt/ room-worker/sysmsg.go room-worker/sysmsg_test.go room-worker/handler.go
git commit -m "refactor: extract pkg/displayfmt.CombineWithFallback shared by sys-msg and enrichment"
```

---

## Task 8: Extend pipelines to match `deptId` in `pkg/pipelines/member.go`

**Files:**
- Modify: `pkg/pipelines/member.go`
- Test: `room-worker/integration_test.go`

**Approach:** Add the second `$or` clause to `matchCandidates` (used by `GetCapacityCheckPipeline` and `GetAddMemberCandidatesPipeline`) and to the existing `GetNewMembersPipeline`. Verify both paths via integration tests.

- [ ] **Step 1: Add the failing integration test for dept-matching**

Add to `room-worker/integration_test.go`:

```go
func TestMongoStore_ListAddMemberCandidates_DeptMatching_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)

	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice", DeptID: "dept-X", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob", SectID: "dept-X", SiteID: "site-a"})

	got, err := store.ListAddMemberCandidates(ctx, []string{"dept-X"}, nil, "room-1")
	require.NoError(t, err)

	accounts := map[string]bool{}
	for _, c := range got {
		accounts[c.Account] = true
	}
	assert.True(t, accounts["alice"], "alice matches by deptId")
	assert.True(t, accounts["bob"], "bob matches by sectId (the orgID coincides)")
	assert.Len(t, got, 2)
}
```

- [ ] **Step 2: Run, verify it fails (alice missing)**

```
make test-integration SERVICE=room-worker -run TestMongoStore_ListAddMemberCandidates_DeptMatching_Integration
```

Expected: FAIL — alice is matched only by deptId which the pipeline doesn't yet consider.

- [ ] **Step 3: Extend `matchCandidates` and `GetNewMembersPipeline` in `pkg/pipelines/member.go`**

In `matchCandidates`, update the org-branch appending:

```go
	if len(orgIDs) > 0 {
		orFilter = append(orFilter, bson.M{"sectId": bson.M{"$in": orgIDs}})
		orFilter = append(orFilter, bson.M{"deptId": bson.M{"$in": orgIDs}})
	}
```

In `GetNewMembersPipeline` (the existing function), make the same change at the analogous location — the existing `orFilter` construction at lines 29-35 of the current file.

- [ ] **Step 4: Run integration tests**

```
make test-integration SERVICE=room-worker
make test-integration SERVICE=room-service
```

Both pass. The new test now succeeds.

- [ ] **Step 5: Commit**

```
git add pkg/pipelines/member.go room-worker/integration_test.go
git commit -m "feat(pipelines): match deptId alongside sectId in candidate $or"
```

---

## Task 9: Update `OrgMemberStatus` + `GetOrgMembersWithIndividualStatus` for dept fields and `member.id` lookup

**Files:**
- Modify: `room-worker/store.go`, `room-worker/store_mongo.go`, `room-worker/integration_test.go`
- Regenerate: `room-worker/mock_store_test.go`

**Approach:** Slim `OrgMemberStatus` to `(Name, TCName, IsDept, …)` — the pipeline resolves per-row sect-vs-dept selection. Switch the `room_members` lookup from `member.account` to `member.id` (covered by the existing unique index). Integration test verifies all four shapes.

- [ ] **Step 1: Add the failing integration test**

Add to `room-worker/integration_test.go`:

```go
func TestMongoStore_GetOrgMembersWithIndividualStatus_DeptAndSect_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)

	mustInsertUser(t, db, &model.User{
		ID: "u_alice", Account: "alice", SiteID: "site-a",
		DeptID: "X", DeptName: "Engineering", DeptTCName: "工程部",
	})
	mustInsertUser(t, db, &model.User{
		ID: "u_bob", Account: "bob", SiteID: "site-a",
		SectID: "X", SectName: "Eng Sect", SectTCName: "工程組",
	})

	const roomID = "room-1"
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_alice", Account: "alice"},
		RoomType: model.RoomTypeChannel,
	})
	// Bob has an individual room_members row (member.id = user._id).
	_, err := db.Collection("room_members").InsertOne(ctx, model.RoomMember{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID,
		Member: model.RoomMemberEntry{ID: "u_bob", Type: model.RoomMemberIndividual, Account: "bob"},
	})
	require.NoError(t, err)

	got, err := store.GetOrgMembersWithIndividualStatus(ctx, roomID, "X")
	require.NoError(t, err)

	byAccount := map[string]OrgMemberStatus{}
	for _, m := range got {
		byAccount[m.Account] = m
	}
	require.Len(t, byAccount, 2)
	assert.Equal(t, OrgMemberStatus{
		Account: "alice", SiteID: "site-a",
		Name: "Engineering", TCName: "工程部", IsDept: true, HasIndividualMembership: false,
	}, byAccount["alice"])
	assert.Equal(t, OrgMemberStatus{
		Account: "bob", SiteID: "site-a",
		Name: "Eng Sect", TCName: "工程組", IsDept: false, HasIndividualMembership: true,
	}, byAccount["bob"])
}
```

- [ ] **Step 2: Run, verify it fails**

```
make test-integration SERVICE=room-worker -run TestMongoStore_GetOrgMembersWithIndividualStatus_DeptAndSect_Integration
```

Expected: FAIL — `OrgMemberStatus` doesn't have `Name`/`TCName`/`IsDept`.

- [ ] **Step 3: Update `OrgMemberStatus` in `room-worker/store.go`**

Replace the existing struct (around line 28-33):

```go
type OrgMemberStatus struct {
	Account                 string `bson:"account"`
	SiteID                  string `bson:"siteId"`
	Name                    string `bson:"name"`
	TCName                  string `bson:"tcName"`
	IsDept                  bool   `bson:"isDept"`
	HasIndividualMembership bool   `bson:"hasIndividualMembership"`
}
```

- [ ] **Step 4: Regenerate mocks**

```
make generate SERVICE=room-worker
```

- [ ] **Step 5: Rewrite `GetOrgMembersWithIndividualStatus` in `room-worker/store_mongo.go`**

Replace the function body (around line 261-294):

```go
func (s *MongoStore) GetOrgMembersWithIndividualStatus(ctx context.Context, roomID, orgID string) ([]OrgMemberStatus, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"$or": bson.A{
			bson.M{"sectId": orgID},
			bson.M{"deptId": orgID},
		}}}},
		{{Key: "$addFields", Value: bson.M{
			"isDept": bson.M{"$eq": bson.A{"$deptId", orgID}},
			"name": bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$deptId", orgID}}, "$deptName", "$sectName"}},
			"tcName": bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$deptId", orgID}}, "$deptTCName", "$sectTCName"}},
		}}},
		{{Key: "$lookup", Value: bson.M{
			"from": "room_members",
			"let":  bson.M{"uid": "$_id"},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$rid", roomID}},
					bson.M{"$eq": bson.A{"$member.type", "individual"}},
					bson.M{"$eq": bson.A{"$member.id", "$$uid"}},
				}}}},
				bson.M{"$limit": 1},
			},
			"as": "individualMembership",
		}}},
		{{Key: "$project", Value: bson.M{
			"_id":                     0,
			"account":                 1,
			"siteId":                  1,
			"name":                    1,
			"tcName":                  1,
			"isDept":                  1,
			"hasIndividualMembership": bson.M{"$gt": bson.A{bson.M{"$size": "$individualMembership"}, 0}},
		}}},
	}
	cursor, err := s.users.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, fmt.Errorf("aggregate org members: %w", err)
	}
	defer cursor.Close(ctx)
	var results []OrgMemberStatus
	if err := cursor.All(ctx, &results); err != nil {
		return nil, fmt.Errorf("decode org members: %w", err)
	}
	return results, nil
}
```

- [ ] **Step 6: Run integration tests**

```
make test-integration SERVICE=room-worker -run TestMongoStore_GetOrgMembersWithIndividualStatus_DeptAndSect_Integration
make test-integration SERVICE=room-worker
```

Both pass.

- [ ] **Step 7: Run unit tests; fix compile errors in `room-worker/handler.go` `processRemoveOrg`**

```
make test SERVICE=room-worker
```

Expected: compile errors — `processRemoveOrg` reads `m.SectName` from `OrgMemberStatus`. As a compile-only stub (Task 10 rewrites this block properly), change the iteration:

```go
	sectName := ""
	for _, m := range members {
		if m.Name != "" {
			sectName = m.Name
			break
		}
	}
```

This keeps the existing behavior temporarily (pre-Task-10) — sect-or-dept name, no tiebreak, no orgID fallback yet.

Run again, all green:

```
make test SERVICE=room-worker
```

- [ ] **Step 8: Commit**

```
git add room-worker/store.go room-worker/store_mongo.go room-worker/handler.go room-worker/integration_test.go room-worker/mock_store_test.go
git commit -m "refactor(room-worker): OrgMemberStatus carries (Name,TCName,IsDept); lookup via member.id"
```

---

## Task 10: Rewrite `processRemoveOrg` with dept-first tiebreak + `displayOrg` formatting; drop "missing SectName" permanent error

**Files:**
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`, `room-worker/integration_test.go`

- [ ] **Step 1: Write failing unit tests covering the four cases**

Add to `room-worker/handler_test.go`, modeled on `TestHandler_ProcessRemoveOrg_AllOverlap_SectNameFromUnfiltered` (line ~3928):

```go
func TestHandler_ProcessRemoveOrg_DeptFirstTiebreak(t *testing.T) {
	cases := []struct {
		name       string
		members    []OrgMemberStatus
		wantSect   string  // value placed in MemberRemoved.SectName
		wantContent string // expected Message.Content
	}{
		{
			name: "all sect users",
			members: []OrgMemberStatus{
				{Account: "u1", SiteID: "site-a", Name: "Sect", TCName: "組", IsDept: false, HasIndividualMembership: true},
				{Account: "u2", SiteID: "site-a", Name: "Sect", TCName: "組", IsDept: false, HasIndividualMembership: true},
			},
			wantSect: "Sect 組", wantContent: `"Sect 組" has been removed from the channel`,
		},
		{
			name: "all dept users",
			members: []OrgMemberStatus{
				{Account: "u1", SiteID: "site-a", Name: "Dept", TCName: "部", IsDept: true, HasIndividualMembership: true},
			},
			wantSect: "Dept 部", wantContent: `"Dept 部" has been removed from the channel`,
		},
		{
			name: "mixed — dept wins",
			members: []OrgMemberStatus{
				{Account: "u1", SiteID: "site-a", Name: "Sect", TCName: "組", IsDept: false, HasIndividualMembership: true},
				{Account: "u2", SiteID: "site-a", Name: "Dept", TCName: "部", IsDept: true, HasIndividualMembership: true},
			},
			wantSect: "Dept 部", wantContent: `"Dept 部" has been removed from the channel`,
		},
		{
			name: "all names empty — fall back to orgID",
			members: []OrgMemberStatus{
				{Account: "u1", SiteID: "site-a", Name: "", TCName: "", IsDept: false, HasIndividualMembership: true},
			},
			wantSect: "o1", wantContent: `"o1" has been removed from the channel`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := NewMockSubscriptionStore(ctrl)

			roomID := "r1"
			store.EXPECT().GetOrgMembersWithIndividualStatus(gomock.Any(), roomID, "o1").Return(tc.members, nil)
			// toRemove empty (all have individual membership) → no DeleteSubscriptionsByAccounts expected.
			store.EXPECT().DeleteRoomMember(gomock.Any(), roomID, model.RoomMemberOrg, "o1").Return(nil)
			store.EXPECT().ReconcileMemberCounts(gomock.Any(), roomID).Return(nil)
			store.EXPECT().GetUser(gomock.Any(), "alice").
				Return(&model.User{ID: "u_a", Account: "alice", SiteID: "site-a", EngName: "Alice", ChineseName: "愛"}, nil)

			var published []publishedMsg
			h := &Handler{
				store: store, siteID: "site-a",
				publish: func(_ context.Context, subj string, data []byte, _ string) error {
					published = append(published, publishedMsg{subj: subj, data: data})
					return nil
				},
				keyStore: testKeyStore, keySender: testKeySender,
			}

			req := model.RemoveMemberRequest{RoomID: roomID, Requester: "alice", OrgID: "o1", Timestamp: 1}
			require.NoError(t, h.processRemoveOrg(context.Background(), &req, nil, false))

			sysMsg := findSysMsg(t, published, "site-a", "member_removed")
			assert.Equal(t, tc.wantContent, sysMsg.Content)
			var payload model.MemberRemoved
			require.NoError(t, json.Unmarshal(sysMsg.SysMsgData, &payload))
			assert.Equal(t, tc.wantSect, payload.SectName)
		})
	}
}
```

Note: `publishedMsg`, `findSysMsg`, `testKeyStore`, `testKeySender` are existing test helpers used by `TestHandler_ProcessRemoveOrg_AllOverlap_SectNameFromUnfiltered`.

- [ ] **Step 2: Run, verify it fails**

```
make test SERVICE=room-worker -run TestHandler_ProcessRemoveOrg_DeptFirstTiebreak
```

Expected: FAIL on the dept-wins and orgID-fallback cases.

- [ ] **Step 3: Rewrite the name-harvest block in `room-worker/handler.go` `processRemoveOrg`**

Locate the block at lines 508-523 (sectName harvest + permanent-error guard). Replace with the two-pass resolution:

```go
	// Two-pass resolution: dept-matching rows win on overlap; otherwise first row.
	var name, tcName string
	for _, m := range members {
		if m.IsDept {
			name, tcName = m.Name, m.TCName
			break
		}
	}
	if name == "" && tcName == "" {
		for _, m := range members {
			if !m.IsDept {
				name, tcName = m.Name, m.TCName
				break
			}
		}
	}
	if name == "" && tcName == "" {
		slog.Warn("org-remove: no name resolved from any member; falling back to orgID", "roomID", req.RoomID, "orgID", req.OrgID)
	}
```

Then update the sys-msg payload + Content (lines 618-631) to use `displayOrg` and the new formatter signature:

```go
	sysMsgPayload, _ := json.Marshal(model.MemberRemoved{
		OrgID:             req.OrgID,
		SectName:          displayOrg(name, tcName, req.OrgID),
		RemovedUsersCount: len(toRemove),
	})
	// … (seed + sysMsg struct unchanged) …
	sysMsg := model.Message{
		ID:          idgen.MessageIDFromRequestID(seed, "rmorg"),
		RoomID:      req.RoomID,
		UserID:      requester.ID,
		UserAccount: requester.Account,
		Type:        model.MessageTypeMemberRemoved,
		Content:     formatRemovedOrg(name, tcName, req.OrgID),
		SysMsgData:  sysMsgPayload,
		CreatedAt:   now,
	}
```

Delete the transitional stub from Task 7/9 (the `sectName := ""` loop and the `formatRemovedOrg(sectName, "", req.OrgID)` call — both replaced above).

- [ ] **Step 4: Run, verify unit tests pass**

```
make test SERVICE=room-worker
```

All cases pass.

- [ ] **Step 5: Commit**

```
git add room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go
git commit -m "feat(room-worker): processRemoveOrg uses dept-first tiebreak with displayOrg + orgID fallback"
```

---

## Task 11: Update enrichment `_orgMatch` lookup in `room-service/store_mongo.go` for prefer-dept + orgId fallback

**Files:**
- Modify: `room-service/store_mongo.go`, `room-service/integration_test.go`

**Approach:** Replace the existing single-field `_orgMatch` lookup (sectId-only) with the spec's full pipeline that unions sectId+deptId, sorts by `_isDept desc`, picks the first `(name, tcName)`, and builds the combined `display` string. The outer `$set` falls back to `member.id` when display is empty.

- [ ] **Step 1: Write the failing integration test**

Add to `room-service/integration_test.go`:

```go
func TestMongoStore_ListRoomMembers_OrgDisplay_DeptFirst_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)
	require.NoError(t, store.EnsureIndexes(ctx))

	const roomID = "room-1"
	mustInsertRoom(t, db, &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a", Name: "R"})
	mustInsertUser(t, db, &model.User{
		ID: "u_alice", Account: "alice", SiteID: "site-a",
		DeptID: "X", DeptName: "Engineering", DeptTCName: "工程部",
	})
	mustInsertUser(t, db, &model.User{
		ID: "u_bob", Account: "bob", SiteID: "site-a",
		SectID: "X", SectName: "Sect", SectTCName: "組",
	})
	_, err := db.Collection("room_members").InsertOne(ctx, model.RoomMember{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID,
		Member: model.RoomMemberEntry{ID: "X", Type: model.RoomMemberOrg},
	})
	require.NoError(t, err)

	got, err := store.ListRoomMembers(ctx, roomID, true)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "Engineering 工程部", got[0].Member.SectName, "dept wins on overlap; name+tcName combined")
}

func TestMongoStore_ListRoomMembers_OrgDisplay_FallbackToOrgId_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)
	require.NoError(t, store.EnsureIndexes(ctx))

	const roomID = "room-1"
	mustInsertRoom(t, db, &model.Room{ID: roomID, Type: model.RoomTypeChannel, SiteID: "site-a", Name: "R"})
	// Org Y has no users at all — display must fall back to the raw orgID.
	_, err := db.Collection("room_members").InsertOne(ctx, model.RoomMember{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID,
		Member: model.RoomMemberEntry{ID: "Y", Type: model.RoomMemberOrg},
	})
	require.NoError(t, err)

	got, err := store.ListRoomMembers(ctx, roomID, true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Y", got[0].Member.SectName, "no matching users → display falls back to member.id")
}
```

- [ ] **Step 2: Run, verify they fail**

```
make test-integration SERVICE=room-service -run "TestMongoStore_ListRoomMembers_OrgDisplay_DeptFirst_Integration|TestMongoStore_ListRoomMembers_OrgDisplay_FallbackToOrgId_Integration"
```

Expected: FAIL — dept branch not yet considered; fallback not in place.

- [ ] **Step 3: Replace the `_orgMatch` lookup + `display.sectName` `$set` in `room-service/store_mongo.go`**

Locate the enrichment block (lines 451-489). Replace the `_orgMatch` `$lookup` pipeline with:

```go
		{{Key: "$lookup", Value: bson.M{
			"from": "users",
			"let": bson.M{
				"orgId": "$member.id",
				"mtyp":  "$member.type",
			},
			"pipeline": bson.A{
				bson.M{"$match": bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$$mtyp", "org"}},
					bson.M{"$or": bson.A{
						bson.M{"$eq": bson.A{"$deptId", "$$orgId"}},
						bson.M{"$eq": bson.A{"$sectId", "$$orgId"}},
					}},
				}}}},
				bson.M{"$addFields": bson.M{
					"_isDept": bson.M{"$eq": bson.A{"$deptId", "$$orgId"}},
					"_name": bson.M{"$cond": bson.A{
						bson.M{"$eq": bson.A{"$deptId", "$$orgId"}}, "$deptName", "$sectName"}},
				"_tcName": bson.M{"$cond": bson.A{
					bson.M{"$eq": bson.A{"$deptId", "$$orgId"}}, "$deptTCName", "$sectTCName"}},
				}},
				bson.M{"$group": bson.M{
					"_id":         nil,
					"isDept":      bson.M{"$max": "$_isDept"},
					"deptName":    bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", "$_name", nil}}},
					"deptTCName":  bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", "$_tcName", nil}}},
					"sectName":    bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", nil, "$_name"}}},
					"sectTCName":  bson.M{"$max": bson.M{"$cond": bson.A{"$_isDept", nil, "$_tcName"}}},
					"memberCount": bson.M{"$sum": 1},
				}},
			},
			"as": "_orgMatch",
		}}},
```

Then update the `$set display` block (existing lines 475-489) so it exposes the raw pair plus memberCount for Go-side resolution:

```go
		{{Key: "$set", Value: bson.M{
			"display": bson.M{
				"engName":     bson.M{"$arrayElemAt": bson.A{"$_userMatch.engName", 0}},
				"chineseName": bson.M{"$arrayElemAt": bson.A{"$_userMatch.chineseName", 0}},
				"isOwner": bson.M{"$in": bson.A{
					"owner",
					bson.M{"$ifNull": bson.A{
						bson.M{"$arrayElemAt": bson.A{"$_subMatch.roles", 0}},
						bson.A{},
					}},
				}},
				"orgRaw":      bson.M{"$arrayElemAt": bson.A{"$_orgMatch", 0}},
				"memberCount": bson.M{"$arrayElemAt": bson.A{"$_orgMatch.memberCount", 0}},
			},
		}}},
```

Then in Go (`room-service/store_mongo.go ListRoomMembers` decode loop, after `cursor.All`), resolve the final `display.sectName` per row by reusing the worker's `combineWithFallback` helper. Move that helper to a shared location both services can import (e.g. `pkg/displayfmt/combine.go`) — simpler than duplicating it:

```go
// pkg/displayfmt/combine.go
package displayfmt

import "strings"

// CombineWithFallback joins first and second with a space, falling back to the non-empty side or the fallback.
func CombineWithFallback(first, second, fallback string) string {
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "" && second == "":
		return fallback
	case first == "":
		return second
	case second == "":
		return first
	case first == second:
		return first
	default:
		return first + " " + second
	}
}
```

Update `room-worker/sysmsg.go` to import and delegate to `pkg/displayfmt.CombineWithFallback` (keep the local `displayName`/`displayOrg` wrappers for the worker's call sites).

In `room-service ListRoomMembers`, after decoding:

```go
for i := range members {
	if members[i].Member.Type != model.RoomMemberOrg {
		continue
	}
	raw := members[i].Display.OrgRaw
	name, tcName := raw.SectName, raw.SectTCName
	if raw.IsDept && raw.DeptName != "" {
		name, tcName = raw.DeptName, raw.DeptTCName
	}
	members[i].Display.SectName = displayfmt.CombineWithFallback(name, tcName, members[i].Member.ID)
	members[i].Display.OrgRaw = nil  // strip from wire output
}
```

This keeps the pipeline at 3 inner stages (vs. the spec's earlier 6-stage variant), eliminates BSON-Go logic duplication, and gives both services a single shared helper for the combine. The `display.sectName` wire shape is unchanged.

- [ ] **Step 4: Run integration tests**

```
make test-integration SERVICE=room-service
```

Both new tests pass; existing enrichment tests continue to pass.

- [ ] **Step 5: Commit**

```
git add room-service/store_mongo.go room-service/integration_test.go
git commit -m "feat(room-service): enrichment prefers dept on overlap, falls back to orgID"
```

---

# Part 3 — Frontend dedup short-circuit

## Task 12: `CreateRoomDialog` navigates directly on `dm already exists` reply

**Files:**
- Modify: `chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.jsx`, `chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.test.jsx`

- [ ] **Step 1: Rewrite the failing test in `CreateRoomDialog.test.jsx`**

Find the existing test at `:139` (`'treats a "dm already exists" reply as success and navigates to the existing room'`). Rewrite it so `summaries` is empty (no synthetic match) and the test asserts `onCreated` is called even though `summaries` never gets mutated:

```jsx
it('navigates directly to the existing room on dm-already-exists reply (no summaries-wait)', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  try {
    const onCreated = vi.fn()
    const onClose = vi.fn()
    const { mocks } = setup({
      summaries: [],  // critical: no pre-populated row; pre-Part-3 code would deadlock on the 3s timeout.
      createRoomResolved: { sync: { error: 'dm already exists', roomId: 'r-existing' } },
      onCreated,
      onClose,
    })

    fireEvent.click(screen.getByLabelText(/Pick people/i))
    // ... rest of the click-through to fire createRoom; mirror the existing test's setup ...
    fireEvent.click(screen.getByRole('button', { name: /Create/i }))

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    expect(onCreated).toHaveBeenCalledWith(expect.objectContaining({ id: 'r-existing', type: 'dm' }))
    expect(onClose).toHaveBeenCalledTimes(1)

    // Advance past the 3-second timeout; banner must NOT appear.
    await vi.advanceTimersByTimeAsync(3500)
    expect(screen.queryByText(/taking longer than expected/i)).toBeNull()
  } finally {
    vi.useRealTimers()
  }
})
```

(Adapt the surrounding fixture setup to match the file's existing `setup()` signature.)

- [ ] **Step 2: Run the test, verify it fails**

```
cd chat-frontend && npm test -- CreateRoomDialog.test
```

Expected: FAIL — current code enters `setPendingRoom`, never matches summaries (empty), hits the 3-second timeout, and `onCreated` is never called.

- [ ] **Step 3: Modify `CreateRoomDialog.jsx` `handleSubmit` to short-circuit on dedup**

Find the block at lines 87-105 in `CreateRoomDialog.jsx`. Replace:

```jsx
      const { sync } = await createRoom(
        nats,
        { name: trimmedName, users: finalUsers, orgs: finalOrgs, channels: finalChannels },
        { treatAsSuccess: isDMExistsReply }
      )
      const roomId = sync.roomId
      const roomType = sync.roomType || (isDMExistsReply(sync) ? 'dm' : undefined)
      const displayName = trimmedName || finalUsers[0] || ''
      setPendingRoom({ id: roomId, type: roomType, siteId: user.siteId, name: displayName })
```

With:

```jsx
      const { sync } = await createRoom(
        nats,
        { name: trimmedName, users: finalUsers, orgs: finalOrgs, channels: finalChannels },
        { treatAsSuccess: isDMExistsReply }
      )
      const roomId = sync.roomId
      const displayName = trimmedName || finalUsers[0] || ''

      if (isDMExistsReply(sync)) {
        // Dedup branch: server already confirmed the DM; skip the summaries-wait that can trip the 3s banner on a BUCKETS_LOADED race.
        onCreated({ id: roomId, type: 'dm', siteId: user.siteId, name: displayName })
        onClose()
        return
      }

      setPendingRoom({ id: roomId, type: sync.roomType, siteId: user.siteId, name: displayName })
```

- [ ] **Step 4: Run the test, verify it passes**

```
cd chat-frontend && npm test -- CreateRoomDialog.test
```

Expected: PASS.

- [ ] **Step 5: Run frontend typecheck + full test suite**

```
cd chat-frontend && npm run typecheck && npm test
```

Both pass.

- [ ] **Step 6: Commit**

```
git add chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.jsx chat-frontend/src/components/MainApp/Sidebar/CreateRoomDialog/CreateRoomDialog.test.jsx
git commit -m "fix(chat-frontend): dedup reply navigates directly without summaries-wait"
```

---

# Final verification

- [ ] **Step 1: Run the full test matrix**

```
make lint
make test
make test-integration
make sast
cd chat-frontend && npm run typecheck && npm test
```

All green.

- [ ] **Step 2: Push the final branch**

```
git push origin claude/fix-member-subscription-bug-QZhjc
```

---

# Part 4 — Remove `Room.CreatedBy` (and rework replay-equivalence)

## Task 13: Drop `CreatedBy` from Room, rewrite duplicate-key check to use requester-sub-exists

**Files:**
- Modify: `pkg/model/room.go`, `pkg/model/model_test.go`
- Modify: `room-worker/handler.go`, `room-worker/handler_test.go`, `room-worker/integration_test.go`
- Modify: `chat-frontend/src/api/types.ts`, `chat-frontend/src/api/fetchSidebarBuckets/index.ts`
- Modify: `docs/client-api.md`

**Approach:** Add the DM-concurrent-create failing test first (proves the current `CreatedBy`-based equivalence check is wrong). Rewrite the duplicate-key blocks in `processCreateRoom` and the sync DM path to use `GetSubscription(requester.Account, room.ID)` instead. Drop the field from the model and all surfaces.

- [ ] **Step 1: Write failing integration test for DM concurrent-create**

Add to `room-worker/integration_test.go`:

```go
func TestHandler_ProcessCreateRoom_DMConcurrentByCounterpart_Integration(t *testing.T) {
	ctx := context.Background()
	db := setupMongo(t)
	store := NewMongoStore(db)
	cap := newPublishCapture(t)
	h := NewHandler(store, "site-a", cap.fn(), testKeyStore, testKeySender)

	mustInsertUser(t, db, &model.User{ID: "u_alice", Account: "alice", EngName: "Alice", ChineseName: "爱", SiteID: "site-a"})
	mustInsertUser(t, db, &model.User{ID: "u_bob", Account: "bob", EngName: "Bob", ChineseName: "鲍", SiteID: "site-a"})

	// Pre-state: alice's worker already raced to create the DM. Room exists + both subs.
	roomID := idgen.BuildDMRoomID("u_alice", "u_bob")
	mustInsertRoom(t, db, &model.Room{
		ID: roomID, Type: model.RoomTypeDM, SiteID: "site-a", Name: "",
		UIDs: []string{"u_alice", "u_bob"}, Accounts: []string{"alice", "bob"},
	})
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_alice", Account: "alice"},
		Name: "bob", RoomType: model.RoomTypeDM,
	})
	mustInsertSub(t, db, &model.Subscription{
		ID: idgen.GenerateUUIDv7(), RoomID: roomID, SiteID: "site-a",
		User: model.SubscriptionUser{ID: "u_bob", Account: "bob"},
		Name: "alice", RoomType: model.RoomTypeDM,
	})

	// Bob's worker now processes Bob's canonical create event (Bob raced too).
	req := model.CreateRoomRequest{
		RoomID: roomID, Users: []string{"alice"},
		RequesterID: "u_bob", RequesterAccount: "bob",
		Timestamp: time.Now().UTC().UnixMilli(),
	}
	data, _ := json.Marshal(req)
	requestID := idgen.GenerateRequestID()
	err := h.processCreateRoom(natsutil.WithRequestID(ctx, requestID), data)
	require.NoError(t, err, "bob's race must not fail with collision; alice's worker already wrote both subs")

	// Exactly one room, exactly one sub per user — no duplicates.
	roomCount, err := db.Collection("rooms").CountDocuments(ctx, bson.M{"_id": roomID})
	require.NoError(t, err)
	assert.Equal(t, int64(1), roomCount)
	subCount, err := db.Collection("subscriptions").CountDocuments(ctx, bson.M{"roomId": roomID})
	require.NoError(t, err)
	assert.Equal(t, int64(2), subCount)
}
```

- [ ] **Step 2: Run, verify it fails with "room ID collision"**

```
make test-integration SERVICE=room-worker -run TestHandler_ProcessCreateRoom_DMConcurrentByCounterpart_Integration
```

Expected: FAIL — `processCreateRoom` returns `newPermanent("room ID collision ...")` because `existing.CreatedBy ("u_alice") != room.CreatedBy ("u_bob")`.

- [ ] **Step 3: Strengthen the test — assert no extra writes**

`BulkCreateSubscriptions` swallows duplicate-key errors silently. A buggy implementation that re-runs the full insert path could still produce `subCount == 2` without exercising the fix. Snapshot the pre-existing sub `_id`s and assert they're unchanged after the call:

```go
// Right before the processCreateRoom call:
type subID struct{ ID string `bson:"_id"` }
var preSubs []subID
cursor, err := db.Collection("subscriptions").Find(ctx, bson.M{"roomId": roomID})
require.NoError(t, err)
require.NoError(t, cursor.All(ctx, &preSubs))
require.Len(t, preSubs, 2)
preIDs := map[string]bool{preSubs[0].ID: true, preSubs[1].ID: true}

// After the call, in addition to the existing assertions:
var postSubs []subID
cursor, err = db.Collection("subscriptions").Find(ctx, bson.M{"roomId": roomID})
require.NoError(t, err)
require.NoError(t, cursor.All(ctx, &postSubs))
require.Len(t, postSubs, 2, "no extra subscription docs created")
for _, s := range postSubs {
    assert.True(t, preIDs[s.ID], "subscription %s replaced — worker should reuse existing", s.ID)
}
```

This makes the test fail under any implementation that double-inserts (even with silent dedup), proving the requester-sub-exists check is actually taking effect.

- [ ] **Step 4: Add the private helper `reconcileRoomOnDuplicateKey` to `room-worker/handler.go`**

Add near the top of the file (after the package-scoped error types, before any handler method). Single source of truth for both call sites:

```go
// reconcileRoomOnDuplicateKey is invoked on CreateRoom duplicate-key errors. It returns the existing room when the requester is a legitimate member (JetStream redelivery, or DM counterpart-raced-first), or a permanent error on a real ID collision.
func (h *Handler) reconcileRoomOnDuplicateKey(ctx context.Context, want *model.Room, requesterAccount string) (*model.Room, error) {
	existing, err := h.store.GetRoom(ctx, want.ID)
	if err != nil {
		return nil, fmt.Errorf("fetch on duplicate-key: %w", err)
	}
	if existing.Type != want.Type || existing.SiteID != want.SiteID {
		return nil, newPermanent("room ID collision (existing type=%s site=%s; want %s/%s)",
			existing.Type, existing.SiteID, want.Type, want.SiteID)
	}
	if _, err := h.store.GetSubscription(ctx, requesterAccount, want.ID); err != nil {
		if errors.Is(err, model.ErrSubscriptionNotFound) {
			return nil, newPermanent("room ID collision (requester %s not a member of existing room %s)",
				requesterAccount, want.ID)
		}
		return nil, fmt.Errorf("check requester sub on duplicate-key: %w", err)
	}
	return existing, nil
}
```

- [ ] **Step 5: Replace the duplicate-key block in `processCreateRoom` (`:1130-1153`) with a helper call**

```go
	if err := h.store.CreateRoom(ctx, room); err != nil {
		if !mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("create room: %w", err)
		}
		existing, err := h.reconcileRoomOnDuplicateKey(ctx, room, requester.Account)
		if err != nil {
			return err
		}
		room = existing
	}
```

- [ ] **Step 6: Replace the duplicate-key block in the sync-DM path (`:1535-1558`) with the helper call**

```go
	if err := h.store.CreateRoom(ctx, room); err != nil {
		if !mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("create room: %w", err)
		}
		existing, err := h.reconcileRoomOnDuplicateKey(ctx, room, requester.Account)
		if err != nil {
			if errors.Is(err, errPermanent) {
				// Sync path needs the client-safe errRoomIDCollision sentinel instead.
				slog.Error("sync DM: room ID collision", "roomID", room.ID, "requester", requester.Account)
				return nil, errRoomIDCollision
			}
			return nil, err
		}
		room = existing
		acceptedAt = existing.CreatedAt  // sync path divergence — kept here at the caller
	}
```

If the existing code maps permanent errors to a sentinel differently, mirror that mapping pattern instead.

- [ ] **Step 7: Drop the `CreatedBy:` field set in both room literal constructions**

In `room-worker/handler.go`, remove the `CreatedBy: requester.ID,` line from the `room := &model.Room{…}` constructions at `:1107` and `:1526`.

- [ ] **Step 8: Drop `CreatedBy` from the model**

In `pkg/model/room.go`, delete the `CreatedBy string `json:"createdBy" bson:"createdBy"`` line.

- [ ] **Step 9: Update all test fixtures referencing `Room.CreatedBy`**

The field is referenced in test fixtures across several files. Find every occurrence before editing — don't trust greps to be exhaustive after manual edits start:

```
grep -rn "CreatedBy" room-worker/ pkg/model/ | grep -v "mock_store_test\|.bak"
```

Expected files (verified at spec time):
- `pkg/model/model_test.go` — Room roundtrip fixture(s). Remove every `CreatedBy:` field set.
- `room-worker/integration_test.go` — at least one positive assertion (e.g. around `:607` `assert.Equal(t, "u_alice", room.CreatedBy)`). Remove the assertion line AND any `CreatedBy:` fixture sets.
- `room-worker/handler_test.go` — any `CreatedBy:` in `&model.Room{…}` fixtures, and any test that asserted a collision based on `CreatedBy` mismatch (rewrite those to either expect success for the DM-concurrent case OR to trigger the new "requester not a member" path with a deliberate unrelated-room setup).

After every file edit, re-run the grep to confirm no `CreatedBy` references remain anywhere in `room-worker/` or `pkg/model/`. The frontend has its own removal in Step 11.

- [ ] **Step 10: Run the Go test matrix**

```
make test SERVICE=room-worker
make test-integration SERVICE=room-worker
make test SERVICE=room-service
make test-integration SERVICE=room-service
make test SERVICE=pkg/model
```

All green. The new DM-concurrent-create test passes; nothing else regresses.

- [ ] **Step 11: Drop from frontend**

`chat-frontend/src/api/types.ts:110` — remove `createdBy: string` from the `Room` interface.
`chat-frontend/src/api/fetchSidebarBuckets/index.ts:135` — remove the `createdBy: '',` line.

Frontend grep: `grep -rnE '\b(createdBy|createdByAccount)\b' chat-frontend/src/`. Neither field exists in the live request shape (the creator's account is taken from the NATS subject); both names should be stripped wherever they appear.

Run:

```
cd chat-frontend && npm run typecheck && npm test
```

All green.

- [ ] **Step 12: Update `docs/client-api.md`**

Strip both names from the doc — neither corresponds to a real field on the wire:

```
grep -rnE '\b(createdBy|createdByAccount)\b' docs/client-api.md
```

Expected matches: the `| createdBy | ... |` / `| createdByAccount | ... |` rows in the Room and Create Room schema tables and the `"createdBy": "..."` / `"createdByAccount": "..."` lines in example JSON blocks. Remove every match; the creator's account is derived server-side from the `{account}` segment of the create-room subject.

- [ ] **Step 13: Commit**

```
git add pkg/model/room.go pkg/model/model_test.go room-worker/handler.go room-worker/handler_test.go room-worker/integration_test.go chat-frontend/src/api/types.ts chat-frontend/src/api/fetchSidebarBuckets/index.ts docs/client-api.md
git commit -m "refactor(room-worker): drop Room.CreatedBy, extract reconcileRoomOnDuplicateKey, use requester-sub-exists for replay equivalence"
```

---

# Part 5 — Remove `target_user` Cassandra column

## Task 14: Drop `TargetUser` from the Cassandra message model and schema

**Files:**
- Modify: `pkg/model/cassandra/message.go`
- Modify: `docker-local/cassandra/init/10-table-messages_by_room.cql`, `11-table-thread_messages_by_room.cql`, `12-table-pinned_messages_by_room.cql`, `13-table-messages_by_id.cql`
- Modify: `history-service/internal/cassrepo/messages_by_room.go`, `history-service/internal/cassrepo/thread_messages.go`
- Modify: `docs/cassandra_message_model.md`, `docs/client-api.md`

**Approach:** Mostly schema/struct simplification, but several test files reference `TargetUser` in fixtures or positive assertions — those must be cleaned up in the same change or the build breaks. Integration tests using testcontainers run the init CQL fresh on each run; they exercise the new schema automatically once the fixtures stop populating the dropped field.

Before editing anything, enumerate every reference:

```
grep -rnE '\bTargetUser\b|\btarget_user\b' pkg/ history-service/ docker-local/ docs/
```

Capture the file list and confirm it matches the expected set below. Anything unexpected gets investigated before you keep going.

- [ ] **Step 1: Drop `TargetUser` from the Go struct**

`pkg/model/cassandra/message.go` — remove the line:

```go
TargetUser            *Participant             `json:"targetUser,omitempty"            cql:"target_user"`
```

- [ ] **Step 2: Drop `target_user` from all four init CQL files**

In each of:
- `docker-local/cassandra/init/10-table-messages_by_room.cql`
- `docker-local/cassandra/init/11-table-thread_messages_by_room.cql`
- `docker-local/cassandra/init/12-table-pinned_messages_by_room.cql`
- `docker-local/cassandra/init/13-table-messages_by_id.cql`

Remove the `target_user FROZEN<"Participant">,` line.

- [ ] **Step 3: Drop `target_user` from history-service `baseColumns`**

`history-service/internal/cassrepo/messages_by_room.go:13` — remove `target_user, ` from the string.
`history-service/internal/cassrepo/thread_messages.go:15` — remove `target_user, ` from the string.

- [ ] **Step 4: Update `docs/cassandra_message_model.md`**

Remove the `target_user FROZEN<"Participant">,` line from all four schema sections (around lines 79, 112, 138, 165).

- [ ] **Step 5: Update `docs/client-api.md`**

Remove the `targetUser` row from the messages schema table (around line 939).

- [ ] **Step 6: Verify history-service docker-compose's inline CQL is consistent**

`history-service/docker-local/docker-compose.yml` has an inline `target_user` declaration at lines 74 and 101 (a duplicate schema for the integration-test stack). Remove `target_user FROZEN<"Participant">,` from both blocks so the dev stack stays consistent with the canonical init files.

- [ ] **Step 7: Clean up test fixtures that reference `TargetUser` / `target_user`**

The Go struct field is gone after Step 1 — any test that still constructs `Message{TargetUser: ...}` or asserts on `.TargetUser` will fail to compile. Confirmed test sites (re-grep before editing to catch any added since):

- `pkg/model/cassandra/message_test.go` — remove `TargetUser:` field set(s) from any `Message` fixture and remove any `assert.Equal/require.NotNil` on `.TargetUser`.
- `history-service/internal/cassrepo/messages_by_id_integration_test.go` — remove `TargetUser:` from insert fixtures and `require.NotNil(t, msg.TargetUser)` + `assert.Equal(...)` blocks (around `:106-108`).
- `history-service/internal/cassrepo/thread_messages_integration_test.go` — same shape (around `:253-255`).
- Any other integration test that pre-seeds rows with a `target_user` CQL column — drop the column from the seed INSERT statement.

Pattern: replace positive assertions on `.TargetUser` with… nothing. The field is gone; there's no "verify it's nil" assertion to write because gocql ignores absent columns and the struct no longer carries them.

- [ ] **Step 8: Run the full test matrix**

```
make lint
make test
make test-integration
```

All green. Integration tests start fresh Cassandra containers with the updated init scripts; production schemas are not touched (ops/IaC migration is out of scope).

- [ ] **Step 9: Commit**

```
git add pkg/model/cassandra/ docker-local/cassandra/init/ history-service/internal/cassrepo/ history-service/docker-local/docker-compose.yml docs/cassandra_message_model.md docs/client-api.md
git commit -m "refactor(cassandra): drop unused target_user column from message schema"
```

# Part 6 — Phantom Org / User Request-Time Validation

Spec: see `2026-05-19-org-to-individual-membership-upgrade-design.md` Part 6.

## Task 15: Reject phantom org IDs and account names at the room-service boundary

Both `member.add` and channel-`create` accepted phantom inputs and silently dropped them at the candidates pipeline — the worker then wrote a `room_members` row and fired a sys-msg for a zero-user org, and async-job results reported `success: true` for a typo'd account. Gate at request time with two new store methods so the synchronous RPC reply carries the error.

- [ ] **Step 1: Extend `RoomStore` in `room-service/store.go`**

```go
// FindExistingOrgIDs returns the subset of orgIDs that match at least
// one user via sectId or deptId. ...
FindExistingOrgIDs(ctx context.Context, orgIDs []string) ([]string, error)

// FindExistingAccounts returns the subset of accounts that have a
// matching user document. ...
FindExistingAccounts(ctx context.Context, accounts []string) ([]string, error)
```

Both no-op (`return nil, nil`) when input is empty so handlers can call them unconditionally without an empty-slice round trip.

Run `make generate SERVICE=room-service` to regenerate `mock_store_test.go`.

- [ ] **Step 2: Implement in `room-service/store_mongo.go`**

`FindExistingOrgIDs` runs two parallel `Distinct` calls (one on `sectId`, one on `deptId`, each filtered by `$in: orgIDs`), unions the results into a set, returns the slice. Both queries ride the existing `(sectId, account)` / `(deptId, account)` compound indexes.

`FindExistingAccounts` is a single `Distinct` on `account` with `$in: accounts`.

- [ ] **Step 3: Add validation helpers in `room-service/handler.go`**

Two methods on `*Handler`:

```go
func (h *Handler) validateOrgIDs(ctx context.Context, orgIDs []string) error
func (h *Handler) validateAccountsExist(ctx context.Context, accounts []string) error
```

Each:
1. No-op on empty input.
2. Call the store method.
3. If `len(existing) == len(input)` — done.
4. Otherwise build a set of `existing`, iterate `input`, and return `fmt.Errorf("org %q: %w", id, errInvalidOrg)` or `fmt.Errorf("user %q: %w", a, errUserNotFound)` for the first missing entry.

`errInvalidOrg` and `errUserNotFound` are already in `sanitizeError`'s allow-list — no helper changes needed.

- [ ] **Step 4: Wire into `handleAddMembers` and `handleCreateRoomChannel`**

Insert the calls immediately after the `allOrgs`/`allUsers` dedup step, before `CountNewMembers` and `publishToStream`:

```go
if err := h.validateOrgIDs(ctx, allOrgs); err != nil {
    return nil, err
}
if err := h.validateAccountsExist(ctx, allUsers); err != nil {
    return nil, err
}
```

Order matters only for which sentinel surfaces first when both dimensions have phantom entries — orgs first by convention. Cheaper checks (bot rejection, restricted-channel, capacity) stay ahead so phantom validation only runs once the request has cleared the no-DB-needed guards.

- [ ] **Step 5: Tests**

Unit (`room-service/handler_test.go`):
- `TestHandler_AddMembers_PhantomOrgRejected` — `Orgs: ["org-nope"]`, store returns empty, assert `errors.Is(err, errInvalidOrg)` and no publish.
- `TestHandler_AddMembers_PartiallyInvalidOrgRejected` — mixed; whole request rejects.
- `TestHandler_AddMembers_NoOrgsSkipsOrgValidation` — gomock controller fails if `FindExistingOrgIDs` is called for a users-only request.
- `TestHandler_AddMembers_PhantomUserRejected` — `errUserNotFound`.
- `TestHandler_AddMembers_NoUsersSkipsUserValidation` — symmetric guard.

Add a shared helper at the top of `handler_test.go`:

```go
func expectAllAccountsExist(store *MockRoomStore) *gomock.Call {
    return store.EXPECT().FindExistingAccounts(gomock.Any(), gomock.Any()).
        DoAndReturn(func(_ context.Context, accs []string) ([]string, error) { return accs, nil })
}
```

12 existing happy-path tests that reach `CountNewMembers` (5 in member.add, 7 in create-channel/create-room) need an `expectAllAccountsExist(store)` line inserted between `GetRoom` / `GetUser` and `CountNewMembers`. Tests that fail earlier (DM-rejected, restricted-non-owner, name-required, direct-bot-rejected) do not.

Integration (`room-service/integration_test.go`):
- `TestMongoStore_FindExistingOrgIDs_Integration` — sectId+deptId set union, all-phantom, empty input, dept-only invariant.
- `TestMongoStore_FindExistingAccounts_Integration` — matching subset, all-phantom, empty input.

- [ ] **Step 6: Update `docs/client-api.md`**

Add Members → Error response: note `"invalid org"` for phantom orgs and `"user not found"` for phantom accounts.

Create Room → Error response: same note (channel branch only — DM/botDM paths don't go through these gates).

- [ ] **Step 7: Verify**

```
make generate SERVICE=room-service
make lint
make test
go vet -tags integration ./room-service/...
```

Rebuild `room-service` only — no other service has runtime code changes:

```
docker compose -f docker-local/compose.services.yaml up -d --build --no-deps room-service
```

Frontend has no hardcoded references to either error string; the existing error envelope renderer surfaces both `"invalid org"` and `"user not found"` to the user without changes.

- [ ] **Step 8: Commit**

```
git add room-service/ docs/client-api.md docs/superpowers/specs/2026-05-19-org-to-individual-membership-upgrade-design.md docs/superpowers/plans/2026-05-19-member-add-improvements-plan.md
git commit -m "feat(room-service): reject phantom org IDs and accounts at request time"
```

# Part 7 — PR #171 Follow-up Findings

Spec: see `2026-05-19-org-to-individual-membership-upgrade-design.md` Part 7. Two review threads from `@mliu33` on PR #171 (merged into `main`, this branch already rebased onto it).

## Task 16: Pass room key pair into `buildAndFanOutRoomKey` (Finding 1)

- [ ] **Step 1: Change the function signature in `room-worker/handler.go:1792`**

```go
func (h *Handler) buildAndFanOutRoomKey(ctx context.Context, roomID string, pair *roomkeystore.VersionedKeyPair, users []model.User) error {
    if pair == nil {
        roomkeymetrics.KeyAbsentErrors.Add(ctx, 1)
        return newPermanentAbsent("room key absent for %s", roomID)
    }
    // ...build event + fanOutKey as today...
}
```

Drop the `keyStore.Get` call at line 1793. Keep the nil check as a defensive guard.

- [ ] **Step 2: Thread `pair` through `finishCreateRoom`**

`finishCreateRoom` at `room-worker/handler.go:1363` is the only site that calls `buildAndFanOutRoomKey` on the create path. Add a `pair *roomkeystore.VersionedKeyPair` parameter, pass it directly to `buildAndFanOutRoomKey` at line 1464.

Update both callers in `processCreateRoom`:
- `room-worker/handler.go:1258` (DM/BotDM branch) — pass `pair` already in scope from L1188.
- `room-worker/handler.go:1360` (channel branch via `processCreateRoomChannel`) — `processCreateRoomChannel` gets a `pair` parameter too; pass through from `processCreateRoom`.

- [ ] **Step 3: `processAddMembers` fetches the pair before the fan-out call**

At `room-worker/handler.go:993-997`:

```go
if len(newSubUsers) > 0 {
    pair, err := h.keyStore.Get(ctx, req.RoomID)
    if err != nil {
        roomkeymetrics.ValkeyErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("op", "Get")))
        return fmt.Errorf("get room key for fan-out: %w", err)
    }
    if err := h.buildAndFanOutRoomKey(ctx, req.RoomID, pair, newSubUsers); err != nil {
        return fmt.Errorf("fan out room key: %w", err)
    }
}
```

`buildAndFanOutRoomKey`'s internal nil check handles the absent case; no separate `if pair == nil` here.

- [ ] **Step 4: Update `TestBuildAndFanOutRoomKey_SendsToAllMembersIncludingRemoteSite`**

At `room-worker/handler_test.go:3324`:
- Drop the `keyStore.EXPECT().Get(...)` expectation.
- Pass `keyPair` directly as the new third argument:
  ```go
  err := h.buildAndFanOutRoomKey(context.Background(), "room-1", keyPair, users)
  ```
- The `keyStore` mock can be dropped entirely from this test (no `Get` call left).

Other tests that exercise `processCreateRoom` / `processAddMembers` end-to-end will still see one `keyStore.Get` per request (the existing gate-Get); they should not need new mocks.

## Task 17: Drop `KeyGenerated` / `KeyRotated` success counters (Finding 2)

- [ ] **Step 1: Delete the four emit sites**

- `room-worker/handler.go:347` — `roomkeymetrics.KeyGenerated.Add(ctx, 1)` (no-prior path)
- `room-worker/handler.go:356` — `roomkeymetrics.KeyGenerated.Add(ctx, 1)` (ErrNoCurrentKey fallback)
- `room-worker/handler.go:362` — `roomkeymetrics.KeyRotated.Add(ctx, 1)` (rotate success)
- `room-service/handler.go:369` — `roomkeymetrics.KeyGenerated.Add(ctx, 1)` (create-time gen success)

- [ ] **Step 2: Remove the declarations from `pkg/roomkeymetrics/metrics.go`**

Delete:
- `KeyGenerated metric.Int64Counter` (line 15)
- `KeyRotated metric.Int64Counter` (line 17)
- The two `init()` blocks that register them (lines 39-45 and 47-53).

Keep `FanoutErrors`, `ValkeyErrors`, `KeyAbsentErrors` as-is.

- [ ] **Step 3: Verify**

```
make lint
make test
```

No tests reference these counters (verified — grep on `_test.go` returns no hits for `KeyGenerated` / `KeyRotated`).

## Task 18: Verify combined Part 7 work

- [ ] **Step 1: Full local check**

```
make lint
make test
go vet -tags integration ./room-worker/... ./room-service/...
```

- [ ] **Step 2: Rebuild affected services**

```
docker compose -f docker-local/compose.services.yaml up -d --build --no-deps room-worker room-service
```

- [ ] **Step 3: Post the two GitHub thread replies**

Each reply text is in the spec, Part 7. Post on the original PR #171 threads.

- [ ] **Step 4: Commit**

```
git add room-worker/handler.go room-worker/handler_test.go room-service/handler.go pkg/roomkeymetrics/metrics.go docs/superpowers/specs/2026-05-19-org-to-individual-membership-upgrade-design.md docs/superpowers/plans/2026-05-19-member-add-improvements-plan.md
git commit -m "refactor(room-worker): address PR #171 follow-up review findings"
```
