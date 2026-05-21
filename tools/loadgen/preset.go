package main

import (
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/roomkeystore"
)

// Distribution names the shape of a per-preset random selection.
type Distribution string

const (
	DistUniform Distribution = "uniform"
	DistMixed   Distribution = "mixed"
	DistZipf    Distribution = "zipf"
)

// Range holds an inclusive min/max for integer quantities like content size.
type Range struct {
	Min int
	Max int
}

// Preset is a named, fully deterministic workload specification.
type Preset struct {
	Name         string
	Users        int
	Rooms        int
	RoomSizeDist Distribution
	SenderDist   Distribution
	ContentBytes Range
	MentionRate  float64
	ThreadRate   float64
}

var builtinPresets = map[string]Preset{
	"small": {
		Name: "small", Users: 10, Rooms: 5,
		RoomSizeDist: DistUniform, SenderDist: DistUniform,
		ContentBytes: Range{Min: 200, Max: 200},
	},
	"medium": {
		Name: "medium", Users: 1000, Rooms: 100,
		RoomSizeDist: DistUniform, SenderDist: DistUniform,
		ContentBytes: Range{Min: 200, Max: 200},
	},
	"large": {
		Name: "large", Users: 10000, Rooms: 1000,
		RoomSizeDist: DistUniform, SenderDist: DistUniform,
		ContentBytes: Range{Min: 200, Max: 200},
	},
	"realistic": {
		Name: "realistic", Users: 1000, Rooms: 100,
		RoomSizeDist: DistMixed, SenderDist: DistZipf,
		ContentBytes: Range{Min: 50, Max: 2000},
		MentionRate:  0.10,
		ThreadRate:   0.05,
	},
}

// BuiltinPreset looks up a preset by name.
func BuiltinPreset(name string) (Preset, bool) {
	p, ok := builtinPresets[name]
	return p, ok
}

// Fixtures is the full seed data for a preset run. RoomKeys are load-test
// fixtures derived deterministically from `seed`, not real secrets.
type Fixtures struct {
	Users         []model.User
	Rooms         []model.Room
	Subscriptions []model.Subscription
	RoomKeys      map[string]roomkeystore.RoomKeyPair
}

var (
	engNameBank     = []string{"Alice Wang", "Bob Chen", "Carol Lee", "Dave Liu", "Eve Zhang"}
	chineseNameBank = []string{"愛麗絲", "鮑勃", "卡蘿", "戴夫", "伊芙"}
)

// BuildFixtures is a pure function of (preset, seed, siteID) producing the
// full fixture set. Two calls with equal inputs produce equal outputs.
func BuildFixtures(p *Preset, seed int64, siteID string) Fixtures {
	r := rand.New(rand.NewSource(seed))
	now := time.Unix(0, 0).UTC() // fixed so output is deterministic

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
	// realistic: last 10% of rooms are DMs
	dmStart := p.Rooms
	if p.RoomSizeDist == DistMixed {
		dmStart = p.Rooms - p.Rooms/10
	}
	for i := 0; i < p.Rooms; i++ {
		rtype := model.RoomTypeChannel
		if i >= dmStart {
			rtype = model.RoomTypeDM
		}
		rooms[i] = model.Room{
			ID:        fmt.Sprintf("room-%06d", i),
			Name:      fmt.Sprintf("room-%d", i),
			Type:      rtype,
			SiteID:    siteID,
			UserCount: 0, // filled after membership
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	var subs []model.Subscription
	for i := range rooms {
		members := pickMembers(r, p, i, p.Rooms, &rooms[i], users)
		rooms[i].UserCount = len(members)
		for j := range members {
			subs = append(subs, model.Subscription{
				ID:       fmt.Sprintf("sub-%s-%s", rooms[i].ID, members[j].ID),
				User:     model.SubscriptionUser{ID: members[j].ID, Account: members[j].Account},
				RoomID:   rooms[i].ID,
				SiteID:   siteID,
				Roles:    []model.Role{model.RoleMember},
				JoinedAt: now,
			})
		}
	}
	roomKeys := make(map[string]roomkeystore.RoomKeyPair, len(rooms))
	for i := range rooms {
		roomKeys[rooms[i].ID] = deterministicRoomKeyPair(r)
	}
	return Fixtures{Users: users, Rooms: rooms, Subscriptions: subs, RoomKeys: roomKeys}
}

// deterministicRoomKeyPair generates a 32-byte room secret from bytes drawn
// from r. The secret is used directly as an AES-256-GCM key by roomcrypto; no
// key derivation step is needed. The name retains "KeyPair" for call-site compatibility.
func deterministicRoomKeyPair(r io.Reader) roomkeystore.RoomKeyPair {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(r, buf); err != nil {
		panic(fmt.Errorf("read deterministic key bytes: %w", err))
	}
	return roomkeystore.RoomKeyPair{PrivateKey: buf}
}

func pickMembers(r *rand.Rand, p *Preset, roomIdx, totalRooms int, room *model.Room, users []model.User) []model.User {
	if room.Type == model.RoomTypeDM {
		// Two distinct users.
		i := r.Intn(len(users))
		j := r.Intn(len(users) - 1)
		if j >= i {
			j++
		}
		return []model.User{users[i], users[j]}
	}
	switch p.RoomSizeDist {
	case DistMixed:
		// 10% of rooms get up to 500 members; rest get 2-20.
		size := 2 + r.Intn(19)
		if r.Intn(10) == 0 {
			size = 2 + r.Intn(499)
		}
		return sampleWithoutReplacement(r, users, size)
	default:
		// Assign each user to exactly one room via round-robin so that every
		// user appears in at least one room.
		var members []model.User
		for i := range users {
			if i%totalRooms == roomIdx {
				members = append(members, users[i])
			}
		}
		if len(members) < 2 {
			// Pad with random extras to ensure at least 2 members.
			extra := sampleWithoutReplacement(r, users, 2)
			seen := make(map[string]bool)
			for i := range members {
				seen[members[i].ID] = true
			}
			for i := range extra {
				if !seen[extra[i].ID] {
					members = append(members, extra[i])
					seen[extra[i].ID] = true
				}
				if len(members) >= 2 {
					break
				}
			}
		}
		return members
	}
}

func sampleWithoutReplacement(r *rand.Rand, users []model.User, n int) []model.User {
	if n > len(users) {
		n = len(users)
	}
	idx := r.Perm(len(users))[:n]
	out := make([]model.User, n)
	for i, k := range idx {
		out[i] = users[k]
	}
	return out
}
