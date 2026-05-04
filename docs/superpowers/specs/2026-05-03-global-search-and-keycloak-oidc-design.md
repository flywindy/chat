# Global Search Bar + Keycloak OIDC Login — Design

**Date:** 2026-05-03
**Branch target:** `claude/sleepy-swartz-0fff19`
**Scope:** `chat-frontend`, `docker-local`. **No backend code changes.**

## 1. Problem

Local-dev users have no way to:

1. Find rooms or messages without manually scrolling the room list.
2. Search inside the currently open room without scrolling its full history.
3. Jump to a specific older message identified by search.
4. Log in via Keycloak (the auth-service's `DEV_MODE=false` path) — the
   frontend only knows the dev-mode account-input flow today, and Keycloak has
   no realm or users provisioned.

Backend capability is already in place: search-service exposes
`searchRooms` and `searchMessages` over NATS; history-service exposes
`LoadSurroundingMessages`; room-service exposes `rooms.create` and
`member.add`. This work is purely frontend integration plus Keycloak realm
seeding for local dev.

## 2. Scope

In scope:

- A. Keycloak realm export with two pre-provisioned users (alice, bob).
- B. Frontend `DEV_MODE` runtime flag — current account-input form when true,
  Keycloak OIDC authorization-code + PKCE flow when false.
- D. Microsoft-Teams-style global search bar in the chat header:
  - Type-ahead room dropdown (debounced).
  - Tabbed Search Results pane (Rooms + Messages) on Enter.
  - In-room `Ctrl+F` search strip backed by search-service.
  - Jump-to-message with `LoadSurroundingMessages` and a "Jump to latest"
    pill while viewing a historical slice.

Out of scope (explicit):

- Server-side code (none needed; backend already supports every flow).
- Message edit/delete UI (frontend gap noted; needs broadcast-worker and
  message-worker fan-out work first — separate spec).
- OIDC silent renew, multi-account, persistent "remember me", logout from
  Keycloak.
- Search-as-you-type on the Messages tab (only on tab click).
- Server-side ES `highlight`-API integration (client-side substring wrap is
  sufficient).
- Mobile / narrow-viewport responsiveness for the new header grid.

## 3. Architecture overview

```
┌──── Frontend (Vite SPA) ──────────────────┐    ┌── Local-dev infra ──┐
│                                            │    │                     │
│  LoginPage ─┬─ DEV_MODE=true ─ /auth ──────┼────┤ auth-service        │
│             └─ DEV_MODE=false ─ Keycloak ──┼────┤ Keycloak (realm:    │
│                                  /auth     │    │   chatapp, users:   │
│                                            │    │   alice, bob)       │
│  ChatPage                                  │    │                     │
│   ├─ chat-header                           │    │                     │
│   │   ├─ "Chat" title                     │    │                     │
│   │   ├─ GlobalSearchBar (centered)       │    │                     │
│   │   │     └─ dropdown ─ search.rooms ───┼────┤ search-service      │
│   │   └─ user / logout                    │    │                     │
│   ├─ InRoomSearchBar (slide-down) ── search.messages(RoomIDs=[cur]) ─┤
│   ├─ chat-body                             │    │                     │
│   │   ├─ Sidebar (RoomList)               │    │                     │
│   │   └─ chat-main, one of:               │    │                     │
│   │        a) MessageArea + JumpToLatestPill                          │
│   │        b) SearchResultsPane (Rooms tab + Messages tab)            │
│   │            ├─ Rooms tab ── search.rooms                           │
│   │            └─ Messages tab ── search.messages                     │
│   └─ jumpToMessage() ── msg.surrounding ──┼────┤ history-service     │
└────────────────────────────────────────────┘    └─────────────────────┘
```

## 4. Detailed design

### 4.1 Keycloak realm + users

**File:** `docker-local/keycloak/chatapp-realm.json` (new).

- Realm: `chatapp` (matches existing `OIDC_ISSUER_URL`).
- Public OIDC client `nats-chat`:
  - PKCE required (`pkce.code.challenge.method = S256`).
  - Standard flow enabled, direct grant disabled.
  - Redirect URIs: `http://localhost:3000/*`, `http://localhost:5173/*`.
  - Web origins: `+`.
- Users:
  - `alice` / password `password` / email `alice@chatapp.local`.
  - `bob` / password `password` / email `bob@chatapp.local`.
  - Credentials use `temporary: false` so no forced reset on first login.

**Compose change (`docker-local/compose.deps.yaml`):**
- Mount `./keycloak/chatapp-realm.json` to
  `/opt/keycloak/data/import/chatapp-realm.json:ro`.
- Change command from `start-dev` to `start-dev --import-realm`.
- Existing health check, env vars, and host port (`8180`) stay.

Keycloak only imports on first run; to reset, `docker compose down -v` and
re-run `docker-local/setup.sh`.

### 4.2 Runtime config (`runtimeConfig.js`)

Three new keys, sourced via the existing
`window.__APP_CONFIG__ → import.meta.env.VITE_* → literal default` chain:

| Key              | Default                                          |
|------------------|--------------------------------------------------|
| `DEV_MODE`       | `'true'`                                         |
| `OIDC_ISSUER_URL`| `'http://localhost:8180/realms/chatapp'`         |
| `OIDC_CLIENT_ID` | `'nats-chat'`                                    |

Plumbed end-to-end through `config.js.template`, `30-render-config.sh`, and
`docker-local/setup.sh` (which writes `chat-frontend/.env.local`).

### 4.3 OIDC client (`src/lib/oidc.js`)

Thin wrapper around `oidc-client-ts`'s `UserManager`:

```js
new UserManager({
  authority: OIDC_ISSUER_URL,
  client_id: OIDC_CLIENT_ID,
  redirect_uri: `${window.location.origin}/oidc-callback`,
  response_type: 'code',
  scope: 'openid profile email',
  loadUserInfo: false,
  monitorSession: false,
  userStore: new WebStorageStateStore({ store: window.localStorage }),
})
```

Exposes `signinRedirect()` and `handleCallback()`. No silent renew. When the
NATS JWT expires, the user re-runs the OIDC flow.

### 4.4 LoginPage and `/oidc-callback`

`LoginPage.jsx` reads `DEV_MODE` and renders:

| `DEV_MODE` | Form contents                                       |
|------------|-----------------------------------------------------|
| `true`     | Account input + Site ID input + Connect (current)   |
| `false`    | Site ID input + "Sign in with Keycloak" button.<br/>Click stashes `siteId` in `sessionStorage` and calls `signinRedirect()`. |

`App.jsx` adds one path check:
```jsx
if (window.location.pathname === '/oidc-callback') return <OidcCallback />
return connected ? <ChatPage/> : <LoginPage/>
```

`OidcCallback.jsx` runs `handleCallback()`, reads stashed `siteId`, calls
`connect({ mode: 'sso', ssoToken, siteId })`, then `replaceState('/')`.

If `handleCallback()` rejects (user denies in Keycloak, state mismatch,
network error) or the subsequent `connect()` rejects, `OidcCallback`
renders an error message with a "Back to login" button that
`replaceState('/')` to the LoginPage. The stashed `siteId` is cleared in
both success and failure paths.

`NatsContext.connect` becomes:
```js
connect({ mode: 'dev', account, siteId })       // existing path
connect({ mode: 'sso', ssoToken, siteId })      // OIDC path
```
Internally selects the `/auth` POST body shape. The NATS keypair generation
and JWT-authenticator wiring stay identical.

### 4.5 Header layout refactor

`chat-header` becomes a 3-column grid: `1fr | 480px | 1fr`.

- Left: "Chat" title.
- Center: `<GlobalSearchBar/>`.
- Right: user account + Logout button.

The existing per-room buttons (Members, Leave Room) move into a new
**per-room header strip** rendered at the top of `chat-main` when a room is
selected. This keeps "global" controls in the global header and "room"
controls scoped to the room.

### 4.6 Global search bar — type-ahead dropdown

**Component:** `src/components/GlobalSearchBar.jsx`.

- 480px-wide input, search icon left, autofocus on `Ctrl/Cmd+K`.
- Debounce: 250ms (`useDebounce.js`).
- Trigger: 2+ chars.
- Request: `chat.user.{account}.request.search.rooms`,
  payload `{SearchText, Scope: 'all', Size: 8, Offset: 0}`.
- Dropdown rendered absolutely positioned below the input:
  - max-height 320px.
  - `overflow-y: auto`, `scrollbar-width: none`,
    `::-webkit-scrollbar { display: none }` (hidden scrollbar).
  - Section label "Rooms".
  - Each row: type icon (`#` channel, `@` DM) + room name (with
    `<mark>` wrap of matched substring) + secondary line (room type and
    member count).
  - Footer hint: `↑↓ navigate · Enter see all results · Esc close`,
    plus total result count.

Keyboard:

- `↑` / `↓` → move active row.
- `Enter` on a row → `setSelectedRoom(row)`, dropdown closes, input clears.
- `Enter` with no active row → main pane swaps to `<SearchResultsPane/>`.
- `Esc` → close dropdown, blur input.
- `Ctrl/Cmd+K` (registered on `window`) → focus input.

Empty state: `"No rooms match 'xyz'. Press Enter to also search messages."`

Match highlighting is client-side and **case-insensitive** for every
surface that wraps query text in `<mark>` (this dropdown, the Rooms tab,
and the Messages tab snippets). Server returns plain hits; the client
applies the wrap. The Ctrl+F strip uses the same case-insensitive wrap on
loaded message text.

### 4.7 Search results pane (post-Enter)

**Components:** `SearchResultsPane.jsx`, `SearchResultsRoomTab.jsx`,
`SearchResultsMessageTab.jsx`.

State lives in a new `SearchContext`:
```js
{
  query,
  rooms:    { hits, total, hasMore, offset },
  messages: { fetched, hits, total, hasMore, offset },
  lastViewedRoomID,
}
```

The pane replaces `chat-main` content; sidebar stays. Top of the pane:

- Title: `Results for "<query>"`.
- Right-aligned `← Back to chat` link → restores `lastViewedRoomID`.
- Tab strip: **Rooms (count)** | **Messages**.
- `Rooms` tab is active by default. It is initially seeded with the
  dropdown's last hits (up to 8). On first scroll past the seeded rows
  (sentinel intersect) it refetches with `Size: 50, Offset: 0` to replace
  the seed; subsequent scrolls increment `Offset` by 50.

Rooms tab:

- Same row layout as dropdown but full-pane width (font size 14px).
- Click → switch to that room.
- Pagination: infinite scroll via `IntersectionObserver` on a sentinel row,
  incrementing `Offset` by `Size` until response < `Size`.

Messages tab — fetched only on first click:

- Request: `chat.user.{account}.request.search.messages`,
  payload `{SearchText, RoomIDs: null, Size: 25, Offset: 0}`.
- Each hit row:
  - Line 1: `# room-name · sender · timestamp` (small, muted).
  - Line 2: snippet with matched substring highlighted client-side.
- Click → `jumpToMessage(roomID, messageID)` (Section 4.9).
- Pagination: same infinite-scroll pattern.

Returning to chat: `← Back to chat`, `Esc`, or re-clicking the global
search bar (which still has the query) re-opens the cached results
without refetch.

### 4.8 In-room search (Ctrl+F)

**Component:** `src/components/InRoomSearchBar.jsx`.

A 40px slide-down strip rendered as the topmost element of `chat-main`
(above the per-room header), mounted when active so the sidebar stays
unaffected. Open via `Ctrl/Cmd+F` while focus is in `chat-main`; close via
`Esc`. CSS `max-height` transition (~150ms).

Strip contents:

- Search input (autofocus).
- Match counter `N of M`.
- `↑` / `↓` arrows (or `Shift+Enter` / `Enter`).
- `Esc` button.

Request: `chat.user.{account}.request.search.messages`,
`{SearchText, RoomIDs: [currentRoomID], Size: 100, Offset: 0}`.

If response total > 100, an inline note "Showing first 100 of N matches —
refine your query" appears at the bottom of the strip.

Highlighting:

- `<mark class="cf-hit">` wraps every matched substring in loaded messages.
- The active match also gets `cf-active` (different background).
- Auto-scroll the active match into view.

Off-buffer hits: stepping to a hit whose message isn't loaded triggers
`jumpToMessage` (Section 4.9), which fetches surrounding context and applies
the active highlight.

### 4.9 Jump-to-message primitive

**File:** `src/lib/jumpToMessage.js`.

Single async function used by:

1. Clicking a hit in the global Messages tab.
2. Stepping to an off-buffer hit in `Ctrl+F`.

```js
async function jumpToMessage(roomID, messageID) {
  if (selectedRoom?.id !== roomID) await switchRoom(roomID)
  const slice = await request(msgSurrounding(account, roomID, siteId), {
    messageID, limit: 50,
  })
  replaceRoomBuffer(roomID, slice)
  scrollToMessage(messageID)
  flashHighlight(messageID)              // 2s yellow → fade
  markRoomAsHistoricalSlice(roomID)      // enables "Jump to latest" pill
}
```

NATS subject builder: add `msgSurrounding(account, roomID, siteId)` →
`chat.user.{account}.request.room.{roomID}.{siteId}.msg.surrounding` to
`subjects.js`, matching the existing handler at
`pkg/subject/subject.go:281`.

Flash highlight: CSS class `.flash-jump`,
`background: rgba(255, 220, 0, 0.25)` fading to transparent over 2s.

### 4.10 Reducer changes (`roomEventsReducer.js`)

Two new fields per room:

- `bufferMode: 'live' | 'historical'` — defaults to `'live'`. Set to
  `'historical'` by `jumpToMessage` and by paging older.
- `pendingLiveMessages: Message[]` — only populated while
  `bufferMode === 'historical'`.

Live `MESSAGE_RECEIVED` action checks `bufferMode`:

- `'live'` → append to `messages` (current behavior).
- `'historical'` → push onto `pendingLiveMessages`; do not touch
  `messages`.

`RESET_TO_LIVE_TAIL` action: clears `pendingLiveMessages`, sets
`bufferMode: 'live'`, kicks `loadHistory()` to refetch the live tail.

### 4.11 Jump-to-latest pill

**Component:** `src/components/JumpToLatestPill.jsx`.

Floating button absolutely positioned at the bottom-center of `chat-main`,
~16px above the message input. Visible only when
`bufferMode === 'historical'`. Text:

- `↓ Jump to latest` — when no pending messages.
- `↓ Jump to latest (N new)` — when `pendingLiveMessages.length > 0`.

Click → dispatches `RESET_TO_LIVE_TAIL`, scrolls to bottom.

Auto-dismiss: when the user scrolls forward via `MoreAfter` until they
naturally reach the live tail, dispatch `RESET_TO_LIVE_TAIL` automatically.

## 5. Commit plan

| # | Commit                                                                            | Slice |
|---|-----------------------------------------------------------------------------------|-------|
| 1 | `chore(docker-local): add Keycloak realm export with alice/bob`                   | A     |
| 2 | `feat(chat-frontend): add DEV_MODE and OIDC runtime config`                       | B     |
| 3 | `feat(chat-frontend): DEV_MODE switch with Keycloak OIDC login`                   | B     |
| 4 | `feat(chat-frontend): centered global search bar with room type-ahead`            | D     |
| 5 | `feat(chat-frontend): search results pane with Rooms and Messages tabs`           | D     |
| 6 | `feat(chat-frontend): jumpToMessage primitive + LoadSurroundingMessages wiring`   | D     |
| 7 | `feat(chat-frontend): Ctrl+F in-room search strip`                                | D     |
| 8 | `feat(chat-frontend): Jump to latest pill for historical slice mode`              | D     |

Each commit independently builds, lints, and passes its own tests.

## 6. Testing

### 6.1 Unit tests (Vitest, matching existing `*.test.jsx` patterns)

| File                              | Coverage                                                                                       |
|-----------------------------------|------------------------------------------------------------------------------------------------|
| `LoginPage.test.jsx`              | Renders correct form per `DEV_MODE`; click stashes `siteId`; calls `signinRedirect`.           |
| `OidcCallback.test.jsx`           | Calls `handleCallback`, then `connect({mode:'sso',...})`, then `replaceState`.                 |
| `oidc.test.js`                    | UserManager constructed with expected config from `runtimeConfig`.                             |
| `GlobalSearchBar.test.jsx`        | 250ms debounce (`vi.useFakeTimers`); request payload; dropdown render; ↑↓/Enter/Esc handling.  |
| `useDebounce.test.js`             | Trailing-edge debounce behavior.                                                               |
| `searchClient.test.js`            | Wraps NATS request/reply for both subjects; error mapping.                                     |
| `SearchResultsPane.test.jsx`      | Tab switching; lazy fetch on Messages tab click; `Back to chat` restores room.                 |
| `SearchResultsRoomTab.test.jsx`   | Renders dropdown-style rows; infinite-scroll fetch on `Offset` increment.                      |
| `SearchResultsMessageTab.test.jsx`| Snippet highlight; click triggers `jumpToMessage`.                                             |
| `InRoomSearchBar.test.jsx`        | Slides in on `Ctrl+F`; counter; off-buffer hit triggers `jumpToMessage`; Esc clears.           |
| `JumpToLatestPill.test.jsx`       | Visible only in historical mode; pending count; click dispatches `RESET_TO_LIVE_TAIL`.         |
| `roomEventsReducer.test.js`       | Live message routes to `pendingLiveMessages` when historical; reset action clears state.       |
| `jumpToMessage.test.js`           | `switchRoom` only when needed; replaces buffer; calls `flashHighlight`.                        |

### 6.2 Backend

No backend code changes; existing tests for `LoadSurroundingMessages`,
`searchRooms`, `searchMessages`, `rooms.create`, and `member.add` cover the
APIs this work consumes.

### 6.3 Manual smoke test

1. `make deps-up && make up`.
2. Open two browsers — sign in as alice@site-A and bob@site-A in
   `DEV_MODE=true`. Repeat after flipping to `DEV_MODE=false` to validate
   the Keycloak path.
3. Alice creates rooms `frontend-team` and `design-front`, adds bob to
   each, sends 30+ messages.
4. Bob types `fro` in the global search bar → dropdown shows both rooms;
   clicks → switches.
5. Bob types `deploy` and presses Enter → results pane: Rooms tab empty,
   Messages tab (on click) shows alice's messages with snippet highlight.
6. Bob clicks a message hit → `chat-main` swaps to that room scrolled to
   the message with brief flash; "Jump to latest" pill is visible.
7. Alice sends 2 more → Bob's pill updates to `(2 new)`; click → pill
   disappears, live tail restored.
8. Bob `Ctrl+F` → strip slides down; types `bundle` → counter shows
   `1 of 1`; types `the` → counter `1 of N`; ↑ / ↓ steps through; an
   off-buffer hit triggers `LoadSurroundingMessages`.
9. `Esc` clears `Ctrl+F`. `Ctrl+K` focuses the global search bar.

## 7. Risks and assumptions

- `oidc-client-ts` works correctly with Keycloak's PKCE flow (default,
  well-supported configuration).
- `SearchMessagesResponse` returns the full message content; client-side
  substring highlighting is acceptable. If ES `highlight`-API support is
  desired later, it is a server-side enhancement, transparent to this PR.
- The 3-column header grid (`1fr | 480px | 1fr`) needs a `min-width: 600px`
  on `.chat-header` so the input doesn't collapse on narrow viewports.
  Mobile responsiveness is explicitly out of scope.

## 8. File inventory

**New (25):**

```
docker-local/keycloak/chatapp-realm.json
chat-frontend/src/lib/oidc.js
chat-frontend/src/lib/oidc.test.js
chat-frontend/src/lib/useDebounce.js
chat-frontend/src/lib/useDebounce.test.js
chat-frontend/src/lib/searchClient.js
chat-frontend/src/lib/searchClient.test.js
chat-frontend/src/lib/jumpToMessage.js
chat-frontend/src/lib/jumpToMessage.test.js
chat-frontend/src/pages/OidcCallback.jsx
chat-frontend/src/pages/OidcCallback.test.jsx
chat-frontend/src/context/SearchContext.jsx
chat-frontend/src/context/SearchContext.test.jsx
chat-frontend/src/components/GlobalSearchBar.jsx
chat-frontend/src/components/GlobalSearchBar.test.jsx
chat-frontend/src/components/SearchResultsPane.jsx
chat-frontend/src/components/SearchResultsPane.test.jsx
chat-frontend/src/components/SearchResultsRoomTab.jsx
chat-frontend/src/components/SearchResultsRoomTab.test.jsx
chat-frontend/src/components/SearchResultsMessageTab.jsx
chat-frontend/src/components/SearchResultsMessageTab.test.jsx
chat-frontend/src/components/InRoomSearchBar.jsx
chat-frontend/src/components/InRoomSearchBar.test.jsx
chat-frontend/src/components/JumpToLatestPill.jsx
chat-frontend/src/components/JumpToLatestPill.test.jsx
```

**Edited (13):**

```
docker-local/compose.deps.yaml
chat-frontend/package.json
chat-frontend/deploy/config.js.template
chat-frontend/deploy/30-render-config.sh
docker-local/setup.sh
chat-frontend/src/lib/runtimeConfig.js
chat-frontend/src/lib/subjects.js
chat-frontend/src/lib/roomEventsReducer.js
chat-frontend/src/context/NatsContext.jsx
chat-frontend/src/pages/LoginPage.jsx
chat-frontend/src/pages/ChatPage.jsx
chat-frontend/src/components/MessageArea.jsx
chat-frontend/src/App.jsx
```
