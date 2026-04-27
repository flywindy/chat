package main

import (
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/stream"
	"github.com/hmchangw/chat/pkg/subject"
)

// inboxBootstrapStreamConfig returns the INBOX stream config with cross-site
// Sources + SubjectTransforms layered on top of the canonical baseline from
// pkg/stream.Inbox. Only used from the bootstrap path in main.go when
// BOOTSTRAP_STREAMS=true. In production, inbox-worker owns INBOX creation
// and search-sync-worker never calls this.
//
// Federation mechanics: for each remote site we add a StreamSource pointing
// at `OUTBOX_{remote}` with a SubjectTransform whose `Source` field acts
// as both the filter and the rewrite source — `outbox.{remote}.to.{siteID}.>`
// is matched against the upstream OUTBOX and rewritten to
// `chat.inbox.{siteID}.aggregate.>` on ingest. (NATS JetStream forbids
// setting `FilterSubject` and `SubjectTransforms` together on the same
// source — they are mutually exclusive options; the transform's `Source`
// covers the filter responsibility.) This lets consumers tell local events
// apart from federated ones by the presence of the `aggregate` segment.
//
// This helper stays local to search-sync-worker because it's bootstrap-only
// — inbox-worker will own an equivalent construction (as a proper feature,
// not a test toggle) when it migrates in its own PR.
//
// Requires NATS Server 2.10+ for SubjectTransforms support.
func inboxBootstrapStreamConfig(siteID string, remoteSiteIDs []string) jetstream.StreamConfig {
	baseline := stream.Inbox(siteID)
	destPattern := fmt.Sprintf("chat.inbox.%s.aggregate.>", siteID)
	sources := make([]*jetstream.StreamSource, 0, len(remoteSiteIDs))
	for _, remote := range remoteSiteIDs {
		sourcePattern := fmt.Sprintf("outbox.%s.to.%s.>", remote, siteID)
		sources = append(sources, &jetstream.StreamSource{
			Name: fmt.Sprintf("OUTBOX_%s", remote),
			SubjectTransforms: []jetstream.SubjectTransformConfig{
				{Source: sourcePattern, Destination: destPattern},
			},
		})
	}
	return jetstream.StreamConfig{
		Name:     baseline.Name,
		Subjects: baseline.Subjects,
		Sources:  sources,
	}
}

// inboxMemberCollection is the shared base for collections that index
// subscription lifecycle events (member_added, member_removed) off the
// INBOX stream. It centralizes stream config and subject filters so
// spotlight and user-room collections only need to implement the
// index-specific parts.
//
// The stream name + local subject pattern come straight from pkg/stream.Inbox
// so there's one canonical definition for every consumer of INBOX.
// Cross-site federation (Sources + SubjectTransforms) is a deployment
// concern owned by whichever service creates the INBOX stream and is
// layered on separately — see inboxBootstrapStreamConfig.
type inboxMemberCollection struct{}

func (b *inboxMemberCollection) StreamConfig(siteID string) jetstream.StreamConfig {
	c := stream.Inbox(siteID)
	return jetstream.StreamConfig{
		Name:     c.Name,
		Subjects: c.Subjects,
	}
}

func (b *inboxMemberCollection) FilterSubjects(siteID string) []string {
	return subject.InboxMemberEventSubjects(siteID)
}

// parseMemberEvent decodes an INBOX message into an OutboxEvent + its
// InboxMemberEvent payload and validates the common preconditions shared by
// all inbox-member collections.
//
// Callers decide how to handle the event-level restricted-room flag.
// The Go↔painless contract is "positive hss means restricted" — i.e.
// `payload.HistorySharedSince != nil && *payload.HistorySharedSince > 0`;
// a nil pointer OR a leaked `&0`/negative pointer is the intentional
// "unrestricted" sentinel.
//   - user-room routes the event into `restrictedRooms{}` on positive
//     HSS, otherwise into `rooms[]`
//   - spotlight indexes the room regardless of HSS; HSS is a
//     message-content access concern, not a room-name discovery one
func parseMemberEvent(data []byte) (*model.OutboxEvent, *model.InboxMemberEvent, error) {
	var evt model.OutboxEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil, nil, fmt.Errorf("unmarshal outbox event: %w", err)
	}
	if evt.Timestamp <= 0 {
		return nil, nil, fmt.Errorf("parse member event: missing timestamp")
	}
	var payload model.InboxMemberEvent
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return nil, nil, fmt.Errorf("unmarshal inbox member event: %w", err)
	}
	return &evt, &payload, nil
}
