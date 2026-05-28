package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/roomkeystore"
)

// HistoryPreset is a fully-deterministic spec for the history-read workload.
type HistoryPreset struct {
	Name             string
	Users            int     // global user pool
	Rooms            int     // rooms to seed
	BaselineSize     int     // members per room
	MessagesPerRoom  int     // top-level messages per room (thread replies are additional)
	MessageSpanDays  int     // span from now back over which to spread messages
	ThreadRate       float64 // 0..1 fraction of top-level messages that become thread parents
	RepliesPerThread int     // replies per thread parent
	ContentBytes     int
}

var builtinHistoryPresets = map[string]HistoryPreset{
	"history-small": {
		Name: "history-small", Users: 50, Rooms: 5, BaselineSize: 10,
		MessagesPerRoom: 100, MessageSpanDays: 1, ThreadRate: 0.0,
		RepliesPerThread: 0, ContentBytes: 200,
	},
	"history-medium": {
		Name: "history-medium", Users: 1000, Rooms: 100, BaselineSize: 20,
		MessagesPerRoom: 5000, MessageSpanDays: 7, ThreadRate: 0.05,
		RepliesPerThread: 3, ContentBytes: 200,
	},
	"history-large": {
		Name: "history-large", Users: 10000, Rooms: 1000, BaselineSize: 30,
		MessagesPerRoom: 50000, MessageSpanDays: 30, ThreadRate: 0.10,
		RepliesPerThread: 3, ContentBytes: 200,
	},
}

// BuiltinHistoryPreset looks up a preset by name.
func BuiltinHistoryPreset(name string) (HistoryPreset, bool) {
	p, ok := builtinHistoryPresets[name]
	return p, ok
}

// ThreadParentRef identifies a thread-parent message in a room. The generator
// uses this to pick valid threadMessageId values for GetThreadMessages requests.
type ThreadParentRef struct {
	MessageID    string
	ThreadRoomID string
}

// plannedMessage is one row queued for Cassandra write.
// Thread replies have ThreadParentID != "" and ThreadRoomID == parent's
// ThreadRoomID. Thread parents (top-level messages with replies) have
// ThreadRoomID set and ThreadParentID == "". Plain top-level messages have
// both fields empty.
type plannedMessage struct {
	RoomID         string
	MessageID      string
	SenderID       string
	SenderAccount  string
	SenderEngName  string
	Content        string
	CreatedAt      time.Time
	ThreadRoomID   string // parent's thread room (set on both parent and replies)
	ThreadParentID string // empty for top-level; parent message ID for replies
	TCount         int    // parent-only: number of replies (0 for plain top-level + replies)
}

// MessagePlan is the deterministic schedule of every message the seeder will
// write. Includes top-level messages and thread replies. Ordering is
// (room, asc by CreatedAt).
type MessagePlan struct {
	Messages []plannedMessage
}

// HistoryFixtures bundles every artifact a history-workload seed produces.
type HistoryFixtures struct {
	Fixtures      Fixtures
	Plan          MessagePlan
	ThreadParents map[string][]ThreadParentRef // roomID -> parents
}

// BuildHistoryFixtures is a pure function of (preset, seed, siteID, now)
// producing the full fixture set + write plan. `now` is the wall-clock anchor
// used for message timestamps: timestamps are anchored to now so the
// history-service floor doesn't clip them, but user/room/subscription identity
// remains deterministic on seed.
func BuildHistoryFixtures(p *HistoryPreset, seed int64, siteID string, now time.Time) HistoryFixtures {
	r := rand.New(rand.NewSource(seed))
	now = now.UTC()
	epoch := time.Unix(0, 0).UTC()

	users := make([]model.User, p.Users)
	for i := 0; i < p.Users; i++ {
		users[i] = model.User{
			ID:          fmt.Sprintf("u-%06d", i),
			Account:     fmt.Sprintf("user-%d", i),
			SiteID:      siteID,
			EngName:     engNameBank[i%len(engNameBank)],
			ChineseName: chineseNameBank[i%len(chineseNameBank)],
		}
	}

	rooms := make([]model.Room, p.Rooms)
	for i := 0; i < p.Rooms; i++ {
		rooms[i] = model.Room{
			ID:        fmt.Sprintf("hroom-%06d", i),
			Name:      fmt.Sprintf("hroom-%d", i),
			Type:      model.RoomTypeChannel,
			SiteID:    siteID,
			UserCount: p.BaselineSize,
			CreatedAt: epoch,
			UpdatedAt: epoch,
		}
	}

	// Per-room members.
	subs := make([]model.Subscription, 0, p.Rooms*p.BaselineSize)
	membersByRoom := make([][]model.User, p.Rooms)
	for i := range rooms {
		size := p.BaselineSize
		if size > len(users) {
			size = len(users)
		}
		perm := r.Perm(len(users))[:size]
		members := make([]model.User, size)
		for j, idx := range perm {
			members[j] = users[idx]
			roles := []model.Role{model.RoleMember}
			if j == 0 {
				roles = []model.Role{model.RoleOwner}
			}
			subs = append(subs, model.Subscription{
				ID:       fmt.Sprintf("sub-%s-%s", rooms[i].ID, users[idx].ID),
				User:     model.SubscriptionUser{ID: users[idx].ID, Account: users[idx].Account},
				RoomID:   rooms[i].ID,
				SiteID:   siteID,
				Roles:    roles,
				JoinedAt: epoch,
			})
		}
		membersByRoom[i] = members
	}

	// Room keys for the Valkey seed step.
	roomKeys := make(map[string]roomkeystore.RoomKeyPair, len(rooms))
	for i := range rooms {
		roomKeys[rooms[i].ID] = deterministicRoomKeyPair(r)
	}

	// Message plan: per room, MessagesPerRoom top-level messages uniformly
	// spaced across [now - span, now] with jitter. Some are marked as thread
	// parents and get RepliesPerThread replies each.
	span := time.Duration(p.MessageSpanDays) * 24 * time.Hour
	plan, threadParents := buildMessagePlan(r, p, &rooms, membersByRoom, now, span)

	// Reflect each room's latest top-level message into Room.LastMsgAt so
	// history-service's `before` cap lands at the true latest message, not at
	// 1970 (which would clip the walk via floor clamp) and not at `now`
	// (which would pass over future-edge buckets that exist only because of
	// jitter).
	latestByRoom := map[string]time.Time{}
	for i := range plan.Messages {
		m := &plan.Messages[i]
		if m.ThreadParentID != "" {
			continue
		}
		if t, ok := latestByRoom[m.RoomID]; !ok || m.CreatedAt.After(t) {
			latestByRoom[m.RoomID] = m.CreatedAt
		}
	}
	for i := range rooms {
		if t, ok := latestByRoom[rooms[i].ID]; ok {
			t := t.UTC()
			rooms[i].LastMsgAt = &t
		}
	}

	return HistoryFixtures{
		Fixtures: Fixtures{
			Users:         users,
			Rooms:         rooms,
			Subscriptions: subs,
			RoomKeys:      roomKeys,
		},
		Plan:          plan,
		ThreadParents: threadParents,
	}
}

// maxReplyOffset bounds how far after the parent a thread reply may land.
// Replies are placed 1..maxReplyOffset minutes after the parent, which keeps
// them in the same Cassandra bucket as the parent for all production bucket
// sizes.
const maxReplyOffset = 10 * time.Minute

// buildMessagePlan lays out top-level messages and their thread replies.
// Top-level messages are spaced uniformly across the span with ±50% jitter on
// the gap so they don't land on bucket boundaries. Thread replies are placed
// 1..maxReplyOffset minutes after their parent. A message is only eligible to
// be a thread parent if its createdAt + maxReplyOffset + 1 minute is still
// before `now` — otherwise its replies would land past `now`.
func buildMessagePlan(
	r *rand.Rand,
	p *HistoryPreset,
	rooms *[]model.Room,
	membersByRoom [][]model.User,
	now time.Time,
	span time.Duration,
) (MessagePlan, map[string][]ThreadParentRef) {
	threadParents := make(map[string][]ThreadParentRef, len(*rooms))
	messages := make([]plannedMessage, 0, len(*rooms)*p.MessagesPerRoom)

	for ri := range *rooms {
		room := &(*rooms)[ri]
		members := membersByRoom[ri]
		if len(members) == 0 {
			continue
		}
		gap := span / time.Duration(p.MessagesPerRoom)
		if gap < 2*time.Millisecond {
			gap = 2 * time.Millisecond
		}
		jitter := gap / 2

		// Pass 1: compute top-level message metadata. Defer thread-parent
		// selection until we know which ordinals are eligible (i.e. createdAt
		// is far enough from `now` for replies to fit before `now`).
		type topLevel struct {
			senderIdx int
			createdAt time.Time
			content   string
		}
		tops := make([]topLevel, p.MessagesPerRoom)
		eligible := make([]int, 0, p.MessagesPerRoom)
		for i := 0; i < p.MessagesPerRoom; i++ {
			senderIdx := r.Intn(len(members))
			baseOffset := span - (time.Duration(i)+1)*gap + gap/2
			j := time.Duration(r.Int63n(int64(2*jitter)+1)) - jitter
			createdAt := now.Add(-baseOffset).Add(j).UTC()
			tops[i] = topLevel{
				senderIdx: senderIdx,
				createdAt: createdAt,
				content:   deterministicContent(r, p.ContentBytes),
			}
			if createdAt.Add(maxReplyOffset + time.Minute).Before(now) {
				eligible = append(eligible, i)
			}
		}

		threadCount := int(float64(p.MessagesPerRoom) * p.ThreadRate)
		if threadCount > len(eligible) {
			threadCount = len(eligible)
		}
		threadSet := make(map[int]bool, threadCount)
		if threadCount > 0 && p.RepliesPerThread > 0 {
			perm := r.Perm(len(eligible))[:threadCount]
			for _, k := range perm {
				threadSet[eligible[k]] = true
			}
		}

		roomParents := make([]ThreadParentRef, 0, threadCount)

		for i := 0; i < p.MessagesPerRoom; i++ {
			top := tops[i]
			sender := members[top.senderIdx]
			msgID := fmt.Sprintf("hmsg-%s-%06d", room.ID, i)

			pm := plannedMessage{
				RoomID:        room.ID,
				MessageID:     msgID,
				SenderID:      sender.ID,
				SenderAccount: sender.Account,
				SenderEngName: sender.EngName,
				Content:       top.content,
				CreatedAt:     top.createdAt,
			}

			if threadSet[i] {
				pm.ThreadRoomID = fmt.Sprintf("tr-%s-%06d", room.ID, i)
				pm.TCount = p.RepliesPerThread
				roomParents = append(roomParents, ThreadParentRef{
					MessageID:    msgID,
					ThreadRoomID: pm.ThreadRoomID,
				})
				messages = append(messages, pm)

				for k := 0; k < p.RepliesPerThread; k++ {
					offset := time.Duration(1+r.Intn(int(maxReplyOffset/time.Minute))) * time.Minute
					replyAt := top.createdAt.Add(offset).UTC()
					replySender := members[r.Intn(len(members))]
					replyID := fmt.Sprintf("hreply-%s-%06d-%02d", room.ID, i, k)
					messages = append(messages, plannedMessage{
						RoomID:         room.ID,
						MessageID:      replyID,
						SenderID:       replySender.ID,
						SenderAccount:  replySender.Account,
						SenderEngName:  replySender.EngName,
						Content:        deterministicContent(r, p.ContentBytes),
						CreatedAt:      replyAt,
						ThreadRoomID:   pm.ThreadRoomID,
						ThreadParentID: msgID,
					})
				}
			} else {
				messages = append(messages, pm)
			}
		}

		if len(roomParents) > 0 {
			threadParents[room.ID] = roomParents
		}
	}

	return MessagePlan{Messages: messages}, threadParents
}

// deterministicContent fills a fixed-size string with deterministic alphanum
// bytes from r. Avoids non-printable bytes so logs stay readable.
func deterministicContent(r *rand.Rand, size int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if size <= 0 {
		return ""
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(buf)
}
