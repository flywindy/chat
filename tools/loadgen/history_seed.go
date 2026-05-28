package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/model/cassandra"
	"github.com/hmchangw/chat/pkg/msgbucket"
)

// historyCassandraTables enumerates the message tables the history workload
// writes. Order does not matter — TRUNCATE is idempotent.
var historyCassandraTables = []string{
	"messages_by_room",
	"messages_by_id",
	"thread_messages_by_thread",
}

// historySeedConcurrency caps the number of in-flight INSERT goroutines during
// the Cassandra seed. Each INSERT touches a different partition (room_id+bucket
// or message_id), so coordinator queuing is the bottleneck, not Cassandra
// throughput. 50 is comfortable for a single-node dev cluster and well under
// the gocql per-host connection pool default.
const historySeedConcurrency = 50

func buildCassParticipant(userID, account, engName string) cassandra.Participant {
	return cassandra.Participant{
		ID:      userID,
		Account: account,
		EngName: engName,
	}
}

// bucketOf is a thin wrapper around msgbucket.Sizer to keep the seed call
// sites readable.
func bucketOf(s msgbucket.Sizer, t time.Time) int64 {
	return s.Of(t)
}

// SeedHistoryCassandra truncates the three message tables and writes every
// row from plan. Idempotent: safe to rerun. siteID is stamped into every row.
func SeedHistoryCassandra(ctx context.Context, session *gocql.Session, sizer msgbucket.Sizer, plan *MessagePlan, siteID string) error {
	for _, tbl := range historyCassandraTables {
		if err := session.Query("TRUNCATE " + tbl).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("truncate %s: %w", tbl, err)
		}
	}

	// Build a parent-createdAt lookup so thread replies stamp the parent's
	// real timestamp in messages_by_id.thread_parent_created_at instead of
	// the zero time.
	parentCreatedAtByID := make(map[string]time.Time, len(plan.Messages))
	for i := range plan.Messages {
		m := &plan.Messages[i]
		if m.ThreadParentID == "" {
			parentCreatedAtByID[m.MessageID] = m.CreatedAt
		}
	}

	sem := make(chan struct{}, historySeedConcurrency)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup

	// On ctx cancellation we stop accepting new work but must wait for the
	// in-flight goroutines to finish — otherwise they outlive the caller's
	// session and may race with session teardown.
	cancelled := false
	for i := range plan.Messages {
		msg := &plan.Messages[i]
		select {
		case <-ctx.Done():
			cancelled = true
		case sem <- struct{}{}:
		}
		if cancelled {
			break
		}
		wg.Add(1)
		go func(msg *plannedMessage) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := writePlannedMessage(ctx, session, sizer, msg, siteID, parentCreatedAtByID); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(msg)
	}
	wg.Wait()
	close(errCh)
	if cancelled {
		return ctx.Err()
	}
	if err, ok := <-errCh; ok {
		return err
	}
	return nil
}

// writePlannedMessage issues the per-row INSERTs for one message. For top-level
// messages (and thread parents) it writes to messages_by_room + messages_by_id;
// for thread replies it writes to messages_by_id + thread_messages_by_thread.
// Thread parents additionally stamp thread_room_id and tcount on the parent
// rows so history-service's GetThreadMessages can resolve the threadRoomID
// from messages_by_id without a separate update step.
func writePlannedMessage(
	ctx context.Context,
	session *gocql.Session,
	sizer msgbucket.Sizer,
	msg *plannedMessage,
	siteID string,
	parentCreatedAtByID map[string]time.Time,
) error {
	sender := buildCassParticipant(msg.SenderID, msg.SenderAccount, msg.SenderEngName)
	bucket := bucketOf(sizer, msg.CreatedAt)

	if msg.ThreadParentID == "" {
		// Top-level message (plain or thread parent).
		var tcount *int
		var threadRoomID string
		if msg.ThreadRoomID != "" {
			t := msg.TCount
			tcount = &t
			threadRoomID = msg.ThreadRoomID
		}
		if err := session.Query(
			`INSERT INTO messages_by_room
			   (room_id, bucket, created_at, message_id, sender, msg, site_id, updated_at, thread_room_id, tcount)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.RoomID, bucket, msg.CreatedAt, msg.MessageID, sender, msg.Content, siteID, msg.CreatedAt, threadRoomID, tcount,
		).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("insert messages_by_room %s: %w", msg.MessageID, err)
		}
		if err := session.Query(
			`INSERT INTO messages_by_id
			   (message_id, created_at, room_id, sender, msg, site_id, updated_at, thread_room_id, tcount)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.MessageID, msg.CreatedAt, msg.RoomID, sender, msg.Content, siteID, msg.CreatedAt, threadRoomID, tcount,
		).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("insert messages_by_id %s: %w", msg.MessageID, err)
		}
		return nil
	}

	// Thread reply. messages_by_id captures the reply for lookups by ID;
	// thread_messages_by_thread captures it for the per-thread partition read.
	// Look up the parent's CreatedAt so the stamp matches production where
	// the parent row already exists when the reply is written.
	parentCreatedAt := parentCreatedAtByID[msg.ThreadParentID]
	if err := session.Query(
		`INSERT INTO messages_by_id
		   (message_id, created_at, room_id, sender, msg, site_id, updated_at, thread_room_id, thread_parent_id, thread_parent_created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.MessageID, msg.CreatedAt, msg.RoomID, sender, msg.Content, siteID, msg.CreatedAt, msg.ThreadRoomID, msg.ThreadParentID, parentCreatedAt,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert reply messages_by_id %s: %w", msg.MessageID, err)
	}
	if err := session.Query(
		`INSERT INTO thread_messages_by_thread
		   (thread_room_id, created_at, message_id, room_id, thread_parent_id, sender, msg, site_id, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ThreadRoomID, msg.CreatedAt, msg.MessageID, msg.RoomID, msg.ThreadParentID, sender, msg.Content, siteID, msg.CreatedAt,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("insert thread_messages_by_thread %s: %w", msg.MessageID, err)
	}
	return nil
}

// TeardownHistoryCassandra truncates the three message tables without
// repopulating. Safe to call on cold tables — TRUNCATE is idempotent.
func TeardownHistoryCassandra(ctx context.Context, session *gocql.Session) error {
	for _, tbl := range historyCassandraTables {
		if err := session.Query("TRUNCATE " + tbl).WithContext(ctx).Exec(); err != nil {
			return fmt.Errorf("truncate %s: %w", tbl, err)
		}
	}
	return nil
}

// buildThreadRoomsFromPlan synthesizes the ThreadRoom Mongo docs that pair
// with the thread parents in plan. Each ThreadRoom's LastMsgAt is set to the
// latest reply's CreatedAt and ReplyAccounts is the unique set of reply
// senders, so the doc looks consistent with what room-worker would produce in
// production after the replies were published.
func buildThreadRoomsFromPlan(plan *MessagePlan, siteID string) []model.ThreadRoom {
	type aggregate struct {
		parentID  string
		parentAt  time.Time
		roomID    string
		lastAt    time.Time
		lastID    string
		accounts  map[string]struct{}
		createdAt time.Time
	}
	byThreadRoom := map[string]*aggregate{}
	// Pass 1: capture parent metadata.
	for i := range plan.Messages {
		m := &plan.Messages[i]
		if m.ThreadParentID != "" || m.ThreadRoomID == "" {
			continue
		}
		byThreadRoom[m.ThreadRoomID] = &aggregate{
			parentID:  m.MessageID,
			parentAt:  m.CreatedAt,
			roomID:    m.RoomID,
			createdAt: m.CreatedAt,
			accounts:  map[string]struct{}{},
		}
	}
	// Pass 2: fold reply metadata into each thread's aggregate.
	for i := range plan.Messages {
		m := &plan.Messages[i]
		if m.ThreadParentID == "" {
			continue
		}
		agg, ok := byThreadRoom[m.ThreadRoomID]
		if !ok {
			continue
		}
		if m.CreatedAt.After(agg.lastAt) {
			agg.lastAt = m.CreatedAt
			agg.lastID = m.MessageID
		}
		agg.accounts[m.SenderAccount] = struct{}{}
	}

	out := make([]model.ThreadRoom, 0, len(byThreadRoom))
	for threadRoomID, agg := range byThreadRoom {
		accounts := make([]string, 0, len(agg.accounts))
		for a := range agg.accounts {
			accounts = append(accounts, a)
		}
		out = append(out, model.ThreadRoom{
			ID:                    threadRoomID,
			ParentMessageID:       agg.parentID,
			ThreadParentCreatedAt: agg.parentAt.UTC(),
			RoomID:                agg.roomID,
			SiteID:                siteID,
			LastMsgAt:             agg.lastAt.UTC(),
			LastMsgID:             agg.lastID,
			ReplyAccounts:         accounts,
			CreatedAt:             agg.createdAt.UTC(),
			UpdatedAt:             agg.lastAt.UTC(),
		})
	}
	return out
}

// SeedThreadRooms drops and repopulates the thread_rooms collection with one
// document per thread parent in plan. Indexes the (roomId, lastMsgAt) and
// (roomId, parentMessageId) tuples, mirroring history-service's mongorepo
// indexes so query plans match production.
func SeedThreadRooms(ctx context.Context, db *mongo.Database, plan *MessagePlan, siteID string) error {
	coll := db.Collection("thread_rooms")
	if err := coll.Drop(ctx); err != nil {
		return fmt.Errorf("drop thread_rooms: %w", err)
	}
	rooms := buildThreadRoomsFromPlan(plan, siteID)
	if err := insertDocs(ctx, coll, rooms); err != nil {
		return err
	}
	if _, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "lastMsgAt", Value: -1}}},
		{Keys: bson.D{{Key: "roomId", Value: 1}, {Key: "parentMessageId", Value: 1}}},
	}); err != nil {
		return fmt.Errorf("create thread_rooms indexes: %w", err)
	}
	return nil
}

// TeardownThreadRooms drops the thread_rooms collection.
func TeardownThreadRooms(ctx context.Context, db *mongo.Database) error {
	if err := db.Collection("thread_rooms").Drop(ctx); err != nil {
		return fmt.Errorf("drop thread_rooms: %w", err)
	}
	return nil
}
