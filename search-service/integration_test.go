//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	natsmod "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsrouter"
	"github.com/hmchangw/chat/pkg/natsutil"
	"github.com/hmchangw/chat/pkg/searchengine"
	"github.com/hmchangw/chat/pkg/subject"
	"github.com/hmchangw/chat/pkg/valkeyutil"
)

// --- Fixture -----------------------------------------------------------------

// ccsFixture is the full stack for cross-cluster integration tests: two ES
// containers on a shared Docker network (with CCS configured from local →
// remote), plus Valkey and NATS, plus the wired search-service router.
//
// localURL / remoteURL are the host-mapped HTTP URLs for seeding; the
// search-service itself sees only localURL. `clientNATS` is the raw NATS
// client used to issue request/reply calls.
type ccsFixture struct {
	localURL   string
	remoteURL  string
	localES    searchengine.SearchEngine
	remoteES   searchengine.SearchEngine
	clientNATS *nats.Conn
}

// setupCCSFixture stands up the whole CCS environment. Total cost is ~ES
// container start × 2 (~60-90s) so tests that use it should reuse via
// TestMain when added.
//
// Every major step emits a `t.Logf` so a CI failure (where raw logs are
// often opaque on public runs) leaves enough breadcrumbs in the `go test`
// output to pinpoint which phase broke.
func setupCCSFixture(t *testing.T) *ccsFixture {
	t.Helper()
	ctx := context.Background()

	t.Logf("CCS fixture: creating docker network")
	nw, err := network.New(ctx)
	require.NoError(t, err, "create docker network")
	t.Cleanup(func() { _ = nw.Remove(ctx) })
	t.Logf("CCS fixture: network %q created", nw.Name)

	t.Logf("CCS fixture: starting remote ES container (alias=es-remote)")
	remoteURL := startESForCCS(t, nw, "es-remote", "remote-cluster")
	t.Logf("CCS fixture: remote ES up at %s", remoteURL)

	t.Logf("CCS fixture: starting local ES container (alias=es-local)")
	localURL := startESForCCS(t, nw, "es-local", "local-cluster")
	t.Logf("CCS fixture: local ES up at %s", localURL)

	// Wire local ES to reach the remote in PROXY mode. Proxy mode opens a
	// single direct connection to the configured address and skips the
	// sniff-then-reconnect dance that sniff mode does — that dance requires
	// each remote node to advertise a reachable publish address, which is
	// fragile when docker containers bind transport on 0.0.0.0 and the
	// publish address defaults to an interface the peer can't route to.
	// Proxy mode is the robust choice for CCS over an ephemeral docker
	// network. Ref: ES docs "Remote cluster settings" → `mode=proxy`.
	t.Logf("CCS fixture: configuring cluster.remote.remote1 (proxy mode → es-remote:9300)")
	putClusterSetting(t, localURL, map[string]any{
		"persistent": map[string]any{
			"cluster.remote.remote1.mode":          "proxy",
			"cluster.remote.remote1.proxy_address": "es-remote:9300",
		},
	})
	t.Logf("CCS fixture: waiting for remote1 to report connected=true (timeout 120s)")
	waitForRemoteConnected(t, localURL, "remote1", 120*time.Second)
	t.Logf("CCS fixture: remote1 connected")

	localEngine, err := searchengine.New(ctx, "elasticsearch", localURL)
	require.NoError(t, err, "build searchengine for local")
	remoteEngine, err := searchengine.New(ctx, "elasticsearch", remoteURL)
	require.NoError(t, err, "build searchengine for remote")

	t.Logf("CCS fixture: starting valkey")
	valkeyAddr := startValkey(t)
	valkeyClient, err := valkeyutil.Connect(ctx, valkeyAddr, "")
	require.NoError(t, err, "connect valkey")
	t.Cleanup(func() { valkeyutil.Disconnect(valkeyClient) })
	t.Logf("CCS fixture: valkey at %s", valkeyAddr)

	t.Logf("CCS fixture: starting NATS")
	natsURL := startNATS(t)
	serverNC, err := natsutil.Connect(natsURL, "")
	require.NoError(t, err, "connect nats (server side)")
	t.Cleanup(func() { _ = serverNC.Drain() })

	clientNC, err := nats.Connect(natsURL)
	require.NoError(t, err, "connect nats (client side)")
	t.Cleanup(func() { clientNC.Close() })
	t.Logf("CCS fixture: NATS at %s", natsURL)

	// Thread the same index name through both the store and handlerConfig
	// so the test exercises the full SEARCH_USER_ROOM_INDEX wiring path
	// (store.GetUserRoomDoc → ES index; query builder → terms-lookup index).
	userRoomIndex := UserRoomIndex
	store := newESStore(localEngine, userRoomIndex)
	cache := newValkeyCache(valkeyClient)
	handler := newHandler(store, cache, handlerConfig{
		DocCounts:               25,
		MaxDocCounts:            100,
		RestrictedRoomsCacheTTL: 5 * time.Minute,
		RecentWindow:            365 * 24 * time.Hour,
		UserRoomIndex:           userRoomIndex,
	})

	router := natsrouter.New(serverNC, "search-service-test")
	router.Use(natsrouter.RequestID())
	handler.Register(router)

	return &ccsFixture{
		localURL:   localURL,
		remoteURL:  remoteURL,
		localES:    localEngine,
		remoteES:   remoteEngine,
		clientNATS: clientNC,
	}
}

// startESForCCS starts one ES node on the shared network with the given
// network alias so the peer can reach it at `{alias}:9300`. Returns the
// host-mapped HTTP URL for seeding.
//
// `transport.host: 0.0.0.0` is required so the transport port binds on all
// interfaces, including the bridge network (ES 8.x defaults to `_site_`
// which excludes the container's bridge IP in some setups). CCS itself
// uses `proxy` mode to avoid publish-address sensitivity — see
// setupCCSFixture. `xpack.security.enabled=false` matches the local dev
// deps compose.
func startESForCCS(t *testing.T, nw *testcontainers.DockerNetwork, alias, clusterName string) string {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "elasticsearch:8.17.0",
			ExposedPorts: []string{"9200/tcp", "9300/tcp"},
			Networks:     []string{nw.Name},
			NetworkAliases: map[string][]string{
				nw.Name: {alias},
			},
			Env: map[string]string{
				"cluster.name":           clusterName,
				"discovery.type":         "single-node",
				"xpack.security.enabled": "false",
				"network.host":           "0.0.0.0",
				"transport.host":         "0.0.0.0",
				"cluster.routing.allocation.disk.threshold_enabled": "false",
				"ES_JAVA_OPTS": "-Xms512m -Xmx512m",
			},
			WaitingFor: wait.ForAll(
				wait.ForHTTP("/").WithPort("9200/tcp").WithStartupTimeout(120*time.Second),
				wait.ForHTTP("/_cluster/health?wait_for_status=yellow&timeout=60s").
					WithPort("9200/tcp").
					WithStartupTimeout(120*time.Second),
			),
		},
		Started: true,
	})
	require.NoError(t, err, "start elasticsearch (%s)", alias)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9200")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

func startValkey(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			Cmd:          []string{"valkey-server", "--save", "", "--appendonly", "no"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start valkey")
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)
	return fmt.Sprintf("%s:%s", host, port.Port())
}

func startNATS(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := natsmod.Run(ctx, "nats:2.11-alpine")
	require.NoError(t, err, "start nats")
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	url, err := c.ConnectionString(ctx)
	require.NoError(t, err, "nats connection string")
	return url
}

// --- Index templates ---------------------------------------------------------

// buildTestTemplate wraps a pattern + property map with single-node-friendly
// index settings (1 shard, 0 replicas, 1s refresh) and `dynamic: false`
// mappings. The templates below hand-roll their property sets so the tests
// remain independent of search-sync-worker's custom-analyzer configuration.
func buildTestTemplate(pattern string, properties map[string]any) json.RawMessage {
	body := map[string]any{
		"index_patterns": []string{pattern},
		"template": map[string]any{
			"settings": map[string]any{
				"index": map[string]any{
					"number_of_shards":   1,
					"number_of_replicas": 0,
					"refresh_interval":   "1s",
				},
			},
			"mappings": map[string]any{
				"dynamic":    false,
				"properties": properties,
			},
		},
	}
	data, _ := json.Marshal(body)
	return data
}

func messageTestTemplate() json.RawMessage {
	return buildTestTemplate("messages-*", map[string]any{
		"messageId":   map[string]any{"type": "keyword"},
		"roomId":      map[string]any{"type": "keyword"},
		"siteId":      map[string]any{"type": "keyword"},
		"userId":      map[string]any{"type": "keyword"},
		"userAccount": map[string]any{"type": "keyword"},
		"content": map[string]any{
			"type": "text",
			"fields": map[string]any{
				"keyword": map[string]any{"type": "keyword"},
			},
		},
		"createdAt":                    map[string]any{"type": "date"},
		"threadParentMessageId":        map[string]any{"type": "keyword"},
		"threadParentMessageCreatedAt": map[string]any{"type": "date"},
		"tshow":                        map[string]any{"type": "boolean"},
	})
}

func userRoomTestTemplate() json.RawMessage {
	return buildTestTemplate(UserRoomIndex, map[string]any{
		"userAccount": map[string]any{"type": "keyword"},
		"rooms": map[string]any{
			"type": "text",
			"fields": map[string]any{
				"keyword": map[string]any{"type": "keyword", "ignore_above": 256},
			},
		},
		"restrictedRooms": map[string]any{"type": "flattened"},
		"roomTimestamps":  map[string]any{"type": "flattened"},
		"createdAt":       map[string]any{"type": "date"},
		"updatedAt":       map[string]any{"type": "date"},
	})
}

// --- HTTP helpers ------------------------------------------------------------

// testHTTPClient is a bounded HTTP client for ES control-plane calls —
// stalled containers shouldn't be able to hang the integration job past
// the per-call deadline. Kept small on purpose: these calls hit localhost
// (docker-mapped port) and are cheap when they succeed.
var testHTTPClient = &http.Client{Timeout: 10 * time.Second}

// putClusterSetting pushes a /_cluster/settings update. Used to configure
// the CCS remote after both clusters are up.
func putClusterSetting(t *testing.T, esURL string, body map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, esURL+"/_cluster/settings", bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := testHTTPClient.Do(req)
	require.NoError(t, err, "put cluster settings")
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "put cluster settings: %s", respBody)
}

// waitForRemoteConnected polls /_remote/info until the given remote cluster
// reports connected=true. CCS registration is async — the settings call
// returns immediately but the transport handshake happens in the
// background. On timeout, the last-seen /_remote/info body is captured in
// the failure message so CI can diagnose whether the remote was ever
// registered, what mode it ended up in, or why it couldn't connect.
func waitForRemoteConnected(t *testing.T, localURL, remoteName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := testHTTPClient.Get(localURL + "/_remote/info")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastBody = string(body)
			var info map[string]struct {
				Connected bool `json:"connected"`
			}
			if json.Unmarshal(body, &info) == nil {
				if entry, ok := info[remoteName]; ok && entry.Connected {
					return
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("remote cluster %q never became connected within %s\nlast /_remote/info body: %s",
		remoteName, timeout, lastBody)
}

// seedDoc PUTs a JSON document into ES, synchronously refreshing the index
// so the next search sees it.
func seedDoc(t *testing.T, esURL, index, id string, doc any) {
	t.Helper()
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", esURL, index, id)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := testHTTPClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Truef(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK,
		"seedDoc %s/%s: status=%d body=%s", index, id, resp.StatusCode, body)
}

// --- Templates on both clusters ---------------------------------------------

func (f *ccsFixture) installTemplates(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	t.Logf("templates: upserting messages_template on local")
	require.NoError(t, f.localES.UpsertTemplate(ctx, "messages_template", messageTestTemplate()),
		"upsert messages_template on local")
	t.Logf("templates: upserting messages_template on remote")
	require.NoError(t, f.remoteES.UpsertTemplate(ctx, "messages_template", messageTestTemplate()),
		"upsert messages_template on remote")
	// user-room is local-only per the search-service architecture.
	t.Logf("templates: upserting user_room_template on local")
	require.NoError(t, f.localES.UpsertTemplate(ctx, "user_room_template", userRoomTestTemplate()),
		"upsert user_room_template on local")
	t.Logf("templates: all upserted")
}

// --- Test --------------------------------------------------------------------

// TestSearchService_SearchMessages_CCS_CrossCluster_Unrestricted verifies
// the core CCS promise: a user's search crosses from the local cluster
// (`messages-*`) to a remote cluster (`*:messages-*`) and the service
// returns the merged result set. Both rooms are unrestricted — they live in
// the user-room doc's `rooms[]` — and the terms-lookup clause handles them
// uniformly regardless of which site hosts the message.
func TestSearchService_SearchMessages_CCS_CrossCluster_Unrestricted(t *testing.T) {
	f := setupCCSFixture(t)
	f.installTemplates(t)

	// --- Seed --------------------------------------------------------------
	//
	// Alice is a member of two unrestricted rooms: one lives on the local
	// site, the other on the remote site. The user-room doc (local-only)
	// lists BOTH in `rooms[]` — the sync-worker would normally populate
	// this via INBOX events; here we seed directly.
	const account = "alice"
	const localRoomID = "room-local-1"
	const remoteRoomID = "room-remote-1"

	now := time.Now().UTC()
	createdAt := now.Add(-time.Hour)
	monthIdx := "messages-" + createdAt.Format("2006-01")

	// user-room doc: unrestricted memberships in both rooms.
	seedDoc(t, f.localURL, UserRoomIndex, account, map[string]any{
		"userAccount":     account,
		"rooms":           []string{localRoomID, remoteRoomID},
		"restrictedRooms": map[string]int64{},
		"roomTimestamps": map[string]int64{
			localRoomID:  createdAt.UnixMilli(),
			remoteRoomID: createdAt.UnixMilli(),
		},
		"createdAt": createdAt.Format(time.RFC3339Nano),
		"updatedAt": createdAt.Format(time.RFC3339Nano),
	})

	// Local message in local room.
	seedDoc(t, f.localURL, monthIdx, "msg-local-1", map[string]any{
		"messageId":   "msg-local-1",
		"roomId":      localRoomID,
		"siteId":      "site-local",
		"userId":      "user-bob",
		"userAccount": "bob",
		"content":     "hello from local",
		"createdAt":   createdAt.Format(time.RFC3339Nano),
	})

	// Remote message in remote room. Same index pattern (`messages-*`) on
	// the remote cluster — CCS resolves the `*:messages-*` segment on the
	// local query.
	seedDoc(t, f.remoteURL, monthIdx, "msg-remote-1", map[string]any{
		"messageId":   "msg-remote-1",
		"roomId":      remoteRoomID,
		"siteId":      "site-remote",
		"userId":      "user-carol",
		"userAccount": "carol",
		"content":     "hello from remote",
		"createdAt":   createdAt.Format(time.RFC3339Nano),
	})

	// --- Search via NATS ---------------------------------------------------
	//
	// Round-trips through the real natsrouter: the handler reads
	// restrictedRooms from Valkey (miss → ES prefetch → Valkey SET), then
	// builds the CCS query against `messages-*,*:messages-*` and parses
	// the merged response.
	req := model.SearchMessagesRequest{SearchText: "hello"}
	reqData, err := json.Marshal(req)
	require.NoError(t, err)

	// Generous timeout: first request is Valkey miss → ES prefetch of
	// user-room doc → CCS fanout → response parse. Tight timeouts mask
	// real latency bugs in integration.
	msg, err := f.clientNATS.Request(subject.SearchMessages(account), reqData, 30*time.Second)
	require.NoError(t, err, "NATS request failed")

	t.Logf("response: %s", msg.Data)

	var resp model.SearchMessagesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode response: %s", msg.Data)

	assert.EqualValues(t, 2, resp.Total, "expected both local + remote hits; got body=%s", msg.Data)
	require.Len(t, resp.Results, 2, "expected 2 hits; got body=%s", msg.Data)

	gotRooms := map[string]string{}
	for _, hit := range resp.Results {
		gotRooms[hit.RoomID] = hit.SiteID
	}
	assert.Equal(t, "site-local", gotRooms[localRoomID], "local message should be present")
	assert.Equal(t, "site-remote", gotRooms[remoteRoomID], "remote message should be present via CCS")
}

// TestSearchService_SearchMessages_CCS_CrossCluster_Restricted verifies
// the restricted-room access-control clauses fire correctly across the
// CCS boundary. Alice is a member of one UNRESTRICTED local room and one
// RESTRICTED remote room with historySharedSince (HSS) set to a specific
// cutoff. The user-room doc (local-only) routes the remote room into
// `restrictedRooms{rid: hssMillis}`.
//
// Seed on the remote cluster covers every branch the query builder
// encodes for restricted rooms:
//
//   - pre-HSS parent                            → MUST NOT match (Clause A: createdAt < hss)
//   - post-HSS parent                           → MUST match    (Clause A)
//   - post-HSS thread reply, tshow=true         → MUST match    (Clause B1: outer gate passes + tshow=true fires B1, even though parent is pre-HSS)
//   - post-HSS thread reply, tshow=false        → MUST NOT match (Clause B fails: outer gate passes but inner OR fails — tshow=false AND parent < hss so B2 also fails)
//
// Plus one unrestricted local parent to prove the two paths interact
// cleanly on the same search. Total expected hits: 3 (local + post-HSS
// remote parent + post-HSS remote reply with tshow=true).
func TestSearchService_SearchMessages_CCS_CrossCluster_Restricted(t *testing.T) {
	f := setupCCSFixture(t)
	f.installTemplates(t)

	const account = "alice"
	const localRoomID = "room-local-unrestricted"
	const remoteRoomID = "room-remote-restricted"

	// Temporal setup:
	//   - hss is the user's join-time bound for the restricted remote room.
	//   - preHSS is 3 hours before hss (so pre-HSS messages are clearly
	//     older than the gate).
	//   - postHSS is 1 hour after hss.
	// All well within the default 1-year `recent_window` so none of them
	// get filtered out by the global createdAt range filter.
	now := time.Now().UTC()
	hss := now.Add(-2 * time.Hour)
	preHSS := hss.Add(-3 * time.Hour)
	postHSS := hss.Add(time.Hour)
	monthIdxFor := func(ts time.Time) string { return "messages-" + ts.Format("2006-01") }

	// user-room doc: local room unrestricted, remote room restricted with hss.
	t.Logf("seed: upserting user-room doc for %s (restricted %s since %s)", account, remoteRoomID, hss.Format(time.RFC3339))
	seedDoc(t, f.localURL, UserRoomIndex, account, map[string]any{
		"userAccount": account,
		"rooms":       []string{localRoomID},
		"restrictedRooms": map[string]int64{
			remoteRoomID: hss.UnixMilli(),
		},
		"roomTimestamps": map[string]int64{
			localRoomID:  now.UnixMilli(),
			remoteRoomID: now.UnixMilli(),
		},
		"createdAt": now.Format(time.RFC3339Nano),
		"updatedAt": now.Format(time.RFC3339Nano),
	})

	// --- LOCAL unrestricted room ----------------------------------------
	// One plain message that should always match via the terms-lookup
	// branch (no HSS involved).
	t.Logf("seed: local unrestricted message in %s", localRoomID)
	seedDoc(t, f.localURL, monthIdxFor(postHSS), "msg-local-1", map[string]any{
		"messageId":   "msg-local-1",
		"roomId":      localRoomID,
		"siteId":      "site-local",
		"userId":      "user-bob",
		"userAccount": "bob",
		"content":     "hello from local",
		"createdAt":   postHSS.Format(time.RFC3339Nano),
	})

	// --- REMOTE restricted room -----------------------------------------
	// Four messages, each exercising one branch of the restricted-room
	// clauses. Pre-HSS parent lives at `msg-remote-pre-parent`; its
	// thread replies reference it via threadParentMessageId +
	// threadParentMessageCreatedAt=preHSS.
	t.Logf("seed: remote pre-HSS parent (MUST NOT match)")
	seedDoc(t, f.remoteURL, monthIdxFor(preHSS), "msg-remote-pre-parent", map[string]any{
		"messageId":   "msg-remote-pre-parent",
		"roomId":      remoteRoomID,
		"siteId":      "site-remote",
		"userId":      "user-carol",
		"userAccount": "carol",
		"content":     "hello pre-hss parent",
		"createdAt":   preHSS.Format(time.RFC3339Nano),
	})

	t.Logf("seed: remote post-HSS parent (Clause A match)")
	seedDoc(t, f.remoteURL, monthIdxFor(postHSS), "msg-remote-post-parent", map[string]any{
		"messageId":   "msg-remote-post-parent",
		"roomId":      remoteRoomID,
		"siteId":      "site-remote",
		"userId":      "user-carol",
		"userAccount": "carol",
		"content":     "hello post-hss parent",
		"createdAt":   postHSS.Format(time.RFC3339Nano),
	})

	// Post-HSS reply to a pre-HSS parent, tshow=true → Clause B1 matches.
	// The reply's own createdAt satisfies Clause B's outer gate
	// (createdAt >= hss); tshow=true then fires B1 regardless of the
	// parent's age. If the outer gate weren't there, a pre-HSS tshow=true
	// reply would leak history the user never had access to.
	t.Logf("seed: remote post-HSS reply with tshow=true, pre-HSS parent (Clause B1 match)")
	seedDoc(t, f.remoteURL, monthIdxFor(postHSS), "msg-remote-reply-tshow", map[string]any{
		"messageId":                    "msg-remote-reply-tshow",
		"roomId":                       remoteRoomID,
		"siteId":                       "site-remote",
		"userId":                       "user-carol",
		"userAccount":                  "carol",
		"content":                      "hello tshow reply",
		"createdAt":                    postHSS.Add(time.Minute).Format(time.RFC3339Nano),
		"threadParentMessageId":        "msg-remote-pre-parent",
		"threadParentMessageCreatedAt": preHSS.Format(time.RFC3339Nano),
		"tshow":                        true,
	})

	// Post-HSS reply to a pre-HSS parent, tshow=false → Clause B rejects.
	// Outer gate passes (reply createdAt >= hss) but the inner OR fails:
	// tshow=false blocks B1 and the parent's pre-HSS createdAt blocks B2.
	t.Logf("seed: remote post-HSS reply without tshow, pre-HSS parent (MUST NOT match)")
	seedDoc(t, f.remoteURL, monthIdxFor(postHSS), "msg-remote-reply-plain", map[string]any{
		"messageId":                    "msg-remote-reply-plain",
		"roomId":                       remoteRoomID,
		"siteId":                       "site-remote",
		"userId":                       "user-carol",
		"userAccount":                  "carol",
		"content":                      "hello plain reply",
		"createdAt":                    postHSS.Add(2 * time.Minute).Format(time.RFC3339Nano),
		"threadParentMessageId":        "msg-remote-pre-parent",
		"threadParentMessageCreatedAt": preHSS.Format(time.RFC3339Nano),
	})

	// --- Search ---------------------------------------------------------
	reqData, err := json.Marshal(model.SearchMessagesRequest{SearchText: "hello"})
	require.NoError(t, err)

	msg, err := f.clientNATS.Request(subject.SearchMessages(account), reqData, 30*time.Second)
	require.NoError(t, err, "NATS request failed")
	t.Logf("response: %s", msg.Data)

	var resp model.SearchMessagesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode response: %s", msg.Data)

	got := map[string]bool{}
	for _, hit := range resp.Results {
		got[hit.MessageID] = true
	}

	// Expected matches:
	assert.True(t, got["msg-local-1"], "local unrestricted message must match via terms-lookup")
	assert.True(t, got["msg-remote-post-parent"], "post-HSS remote parent must match via Clause A (CCS)")
	assert.True(t, got["msg-remote-reply-tshow"], "post-HSS remote reply with tshow=true must match via Clause B1 (CCS)")

	// Expected exclusions:
	assert.False(t, got["msg-remote-pre-parent"], "pre-HSS remote parent must be excluded by Clause A gate")
	assert.False(t, got["msg-remote-reply-plain"], "post-HSS remote reply without tshow + pre-HSS parent must be excluded (outer gate passes; B1 and B2 both fail)")

	assert.EqualValues(t, 3, resp.Total, "expected exactly 3 hits; got body=%s", msg.Data)
	require.Len(t, resp.Results, 3, "expected 3 hits; got body=%s", msg.Data)
}
