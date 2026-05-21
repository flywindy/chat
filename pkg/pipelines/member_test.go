package pipelines

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGetNewMembersPipeline(t *testing.T) {
	t.Run("three stages returned", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		assert.Len(t, got, 3)
	})

	t.Run("$or filter with both orgIDs and directAccounts", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1", "org2"}, []string{"alice"}, "room1", "")
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)

		// sectId + deptId for the one orgID group, plus account = 3 clauses.
		assert.Len(t, orFilter, 3)
		assert.NotNil(t, orFilter[0])
		assert.NotNil(t, orFilter[1])
		assert.NotNil(t, orFilter[2])
	})

	t.Run("bot exclusion via $not regex", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)

		notFilter := match["account"]
		assert.NotNil(t, notFilter)
	})

	t.Run("$or filter contains orgIDs when provided", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1"}, nil, "room1", "")
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)

		// sectId + deptId clauses for the org group.
		assert.Len(t, orFilter, 2)
		sectIdFilter := orFilter[0].(bson.M)
		assert.Contains(t, sectIdFilter, "sectId")
		deptIdFilter := orFilter[1].(bson.M)
		assert.Contains(t, deptIdFilter, "deptId")
	})

	t.Run("$or filter contains directAccounts when provided", func(t *testing.T) {
		got := GetNewMembersPipeline(nil, []string{"alice"}, "room1", "")
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)

		assert.Len(t, orFilter, 1)
		accountFilter := orFilter[0].(bson.M)
		assert.Contains(t, accountFilter, "account")
	})

	t.Run("$lookup stage correct", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		stage1 := got[1].(bson.M)
		lookup := stage1["$lookup"].(bson.M)

		assert.Equal(t, "subscriptions", lookup["from"])
		assert.Equal(t, "existingSub", lookup["as"])
		assert.NotNil(t, lookup["let"])
		assert.NotNil(t, lookup["pipeline"])
	})

	t.Run("$match stage filters empty existingSub array", func(t *testing.T) {
		got := GetNewMembersPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		stage2 := got[2].(bson.M)
		match := stage2["$match"].(bson.M)
		existingSub := match["existingSub"].(bson.M)
		eqA := existingSub["$eq"].(bson.A)
		assert.Len(t, eqA, 0, "compare against the empty array literal")
	})
}

func TestGetNewMembersPipelineEmptyRoomID(t *testing.T) {
	pipe := GetNewMembersPipeline([]string{"org-fx"}, []string{"bob"}, "", "")
	require.NotEmpty(t, pipe)

	hasLookup := false
	for _, stage := range pipe {
		m, ok := stage.(bson.M)
		if !ok {
			continue
		}
		if _, found := m["$lookup"]; found {
			hasLookup = true
		}
	}
	assert.False(t, hasLookup, "empty-roomID branch must drop the subscriptions $lookup")
}

func TestGetNewMembersPipelineWithRoomIDStillHasLookup(t *testing.T) {
	pipe := GetNewMembersPipeline([]string{"org-fx"}, []string{"bob"}, "r1", "")
	require.NotEmpty(t, pipe)

	hasLookup := false
	for _, stage := range pipe {
		m, ok := stage.(bson.M)
		if !ok {
			continue
		}
		if _, found := m["$lookup"]; found {
			hasLookup = true
		}
	}
	assert.True(t, hasLookup, "non-empty roomID must keep the subscriptions $lookup")
}

func TestGetAddMemberCandidatesPipeline(t *testing.T) {
	t.Run("stage count is 4", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		require.NoError(t, err)
		assert.Len(t, got, 4)
	})

	t.Run("$match shape with orgIDs only — 2 OR clauses: sectId, deptId", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline([]string{"org1"}, nil, "room1", "")
		require.NoError(t, err)
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)
		assert.Len(t, orFilter, 2, "sectId + deptId clauses for orgIDs only")
		assert.Contains(t, orFilter[0].(bson.M), "sectId")
		assert.Contains(t, orFilter[1].(bson.M), "deptId")
	})

	t.Run("$match shape with directAccounts only — 1 OR clause for account", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline(nil, []string{"alice"}, "room1", "")
		require.NoError(t, err)
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)
		assert.Len(t, orFilter, 1)
		assert.Contains(t, orFilter[0].(bson.M), "account")
	})

	t.Run("$match shape with both — 3 elements in $or: sectId, deptId, account", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline([]string{"org1"}, []string{"alice"}, "room1", "")
		require.NoError(t, err)
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		orFilter := match["$or"].(bson.A)
		assert.Len(t, orFilter, 3, "sectId + deptId + account")
	})

	t.Run("excludeAccount adds $ne to the account filter", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline([]string{"org1"}, []string{"alice"}, "room1", "exclude-me")
		require.NoError(t, err)
		stage0 := got[0].(bson.M)
		match := stage0["$match"].(bson.M)
		accountFilter := match["account"].(bson.M)
		assert.Equal(t, "exclude-me", accountFilter["$ne"])
	})

	t.Run("error on empty roomID", func(t *testing.T) {
		got, err := GetAddMemberCandidatesPipeline([]string{"org1"}, nil, "", "")
		assert.Nil(t, got)
		assert.ErrorIs(t, err, ErrRoomIDRequired)
	})
}
