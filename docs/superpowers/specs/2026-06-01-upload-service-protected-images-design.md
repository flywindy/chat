# upload-service — Protected-image upload & download (design)

Date: 2026-06-01

> **Update 2 (post-review, authoritative).** Per later PR feedback the API was
> revised; where this note conflicts with the body below, this note wins:
> - **Endpoints** are now `/api/v1`: `POST /api/v1/rooms/:roomId/upload/image`
>   and `GET /api/v1/rooms/:roomId/image/:fileId` (was `/api/v4/.../upload/
>   protected-images` and `/api/v4/.../protected-image/:fileId`).
> - **Single-image upload** (industry standard for user-facing uploads), not
>   batch. One file under the `image` form field; validation failures return a
>   `4xx`/`5xx` errcode (no per-file `results` array). Success returns
>   `{ "name", "relativePath" }` (200), with `relativePath` =
>   `api/v1/rooms/{groupId}/image/{fileId}?drive_host=…`.
> - **Drive client**: `UploadGroupImages([]MultipartFile) []resp` →
>   `UploadGroupImage(MultipartFile) (*resp, error)` (single file via the bulk
>   endpoint, returns the lone result).
> - **Handlers renamed**: `HandleUploadProtectedImages`→`HandleUploadImage`,
>   `HandleDownloadProtectedImage`→`HandleDownloadImage`.
> - **Max image size is configurable**: `MAX_IMAGE_SIZE_BYTES` env (default
>   `26214400` = 25 MiB), validated in the handler. The `drive.UploadImageMaxSizeBytes`
>   constant and the `MAX_FILES` env var were removed.
>
> **Update 3 (post-review, authoritative).** The single-image upload from Update 2
> was reverted to the **batch** API (partial-success `results` array), keeping the
> `/api/v1` move and configurable size:
> - `POST /api/v1/rooms/:roomId/upload/images` (plural), form field `images`
>   (repeatable), handler `HandleUploadImages`. Download endpoint unchanged.
> - Per-file validation (size/type/open) yields per-file `failure` entries in the
>   `200` `{ "results": [ { name, status, error?, relativePath? } ] }` body;
>   `relativePath` = `api/v1/rooms/{groupId}/image/{fileId}?drive_host=…`. Whole-
>   request failures (missing roomId, non-multipart, **too many files**, no
>   email, membership, room-not-found, Drive transport) use the errcode envelope.
> - Drive client back to `UploadGroupImages([]MultipartFile) ([]resp, error)`
>   (bulk). `preprocessFiles` restored.
> - **Both** size knobs apply: `MAX_FILES` (count, default 10) **and**
>   `MAX_IMAGE_SIZE_BYTES` (per-image bytes, default 25 MiB).

## 1. Summary

A new flat `package main` HTTP service at the repo root, `upload-service/`, that
proxies protected inline images to/from an internal Drive. It exposes two
authenticated, room-scoped HTTP endpoints:

- `POST /api/v1/rooms/:roomId/upload/image` — upload a single image on behalf
  of a room. Protected images (unlike public ones) are stored with
  user-ownership metadata and later served via signed URLs.
- `GET /api/v1/rooms/:roomId/image/:fileId` — download a single protected
  image. The service acts as a proxy: it asks Drive for a signed URL, streams
  the bytes from that URL, and pipes them straight back to the client.

The service is **HTTP-in, Mongo + Drive-HTTP-out** — it does **not** use NATS or
JetStream. It follows the `auth-service` Gin pattern (`gin.New()` +
recovery/request-id/access-log middleware) and the standard per-service file
layout.

A reusable Drive API client lives in a new `pkg/drive` package.

## 2. Components

### 2.1 `pkg/drive`

A self-contained Drive HTTP client. Three files.

**`config.go`**

```go
type Config struct {
    URL               string            `env:"URL"`
    Token             string            `env:"API_TOKEN"`
    BaseURLConfigPath string            `env:"BASE_URL_CONFIG_PATH" envDefault:"etc/config/baseurls.json"`
    BaseURLMap        map[string]string
}
```

`(*Config) LoadBaseURLs() ` reads and JSON-parses the file at
`BaseURLConfigPath` into `BaseURLMap` (a `map[string]string` of
`siteID -> drive base URL`). On a missing file or invalid JSON it **logs a
warning via `slog` and initializes an empty map**, so the service starts with
default values rather than failing.

**`images_file.go`**

```go
const UploadImageMaxSizeBytes int64 = 25 * 1024 * 1024 // 25 MiB

var AllowedImageFileTypes = map[string]bool{
    ".png": true, ".jpeg": true, ".jpg": true, ".heic": true,
}

// GroupImageObject is Drive's per-file descriptor in a bulk-upload response.
type GroupImageObject struct {
    FileID   string `json:"objectId"`
    GroupID  string `json:"groupId"`
    Filename string `json:"fileName"`
}

// UploadGroupImageResponse is one item in the Drive bulk-upload response.
type UploadGroupImageResponse struct {
    Status string           `json:"status"`
    File   GroupImageObject `json:"object"`
    Error  string           `json:"error,omitempty"`
}

// GetGroupImageResponse carries a streamed download body + metadata.
type GetGroupImageResponse struct {
    Reader        io.ReadCloser
    ContentType   string
    ContentLength int64
}

// UploadProtectedImageResponse is one item in the HTTP upload response.
type UploadProtectedImageResponse struct {
    Name         string `json:"name"`
    Status       string `json:"status"`
    Error        string `json:"error,omitempty"`
    RelativePath string `json:"relativePath,omitempty"`
}

// UploadImageResponse is the public-image equivalent (kept for parity; unused
// by the protected-image flow).
type UploadImageResponse struct {
    Name       string `json:"name"`
    Status     string `json:"status"`
    Error      string `json:"error,omitempty"`
    PreviewURL string `json:"previewUrl,omitempty"`
}

// ImagesFormData accumulates files staged for upload.
type ImagesFormData struct{ files []File }
func NewImagesForm() *ImagesFormData
func (f *ImagesFormData) AddFile(filename, contentType string, r io.Reader)
func (f *ImagesFormData) FileCount() int
func (f *ImagesFormData) ClearFiles()
func (f *ImagesFormData) Files() []File
```

> **Note on the spec's struct list.** The original task listed the
> Reader/ContentType/ContentLength struct under the name
> `UploadGroupImageResponse`, but `GetGroupImage` returns
> `*GetGroupImageResponse` and the upload-conversion code reads
> `.Status`/`.File`/`.Error`. We therefore split them as above:
> `UploadGroupImageResponse` is the bulk-upload item; `GetGroupImageResponse`
> is the download payload. Pseudo-code lowercase `error` fields are normalized
> to exported `Error string` with json tags.

**`uploader.go`**

```go
type File struct {
    Reader      io.Reader
    Filename    string
    ContentType string
}

type MultipartFile struct {
    File     multipart.File
    Filename string
}

type Client struct {
    uploadClient   *resty.Client
    downloadClient *resty.Client
    baseURLMap     map[string]string
    baseURL        string
    apiToken       string
}
```

- `NewClient(cfg *Config) *Client` — builds `uploadClient` and `downloadClient`
  via `restyutil` (preserving the OTel transport + slog request/response
  logging). Both use a transport with `InsecureSkipVerify` (mandated by the
  spec; carries a justified `// #nosec G402 -- internal Drive over private
  network, TLS skip is required by deployment` directive). `downloadClient`
  gets a **5-minute** timeout (`restyutil.WithTimeout`). `baseURL` ← `cfg.URL`,
  `apiToken` ← `cfg.Token`, `baseURLMap` ← `cfg.BaseURLMap`.
- `(*Client) GetBaseURL() string` — returns `c.baseURL`.
- `(*Client) GetBaseURLFromRoomOrigin(origin string) string` — returns
  `baseURLMap[origin]`, falling back to `c.baseURL` when the origin is absent.
- `(*Client) UploadGroupImages(userID, username, email, groupID, origin string, files []MultipartFile) ([]UploadGroupImageResponse, error)`
  - Header `api-token: <apiToken>`.
  - Multipart form fields: `userId=userID`, `username=username`, `email=email`.
  - Indexed file fields per file `i`: `files[i].file` (the file bytes),
    `files[i].fileName`, and a hardcoded `files[i].mode = "Normal"`.
  - `POST {GetBaseURLFromRoomOrigin(origin)}/api/v1/groups/{groupID}/files/bulk`.
  - Returns the parsed `[]UploadGroupImageResponse`.
- `(*Client) fetchPresignedURL(host, fileID, groupID string) (string, error)`
  - `GET {host}/api/v1/groups/{groupId}/files/{fileId}` with `api-token` header,
    path params, result decoded into `{ url, error }`.
  - Network error → `fmt.Errorf("network error calling signer service: %w", err)`.
  - `resp.IsError()` → `fmt.Errorf("signer service returned status %d: %s", code, result.Error)`.
  - Empty `url` → `fmt.Errorf("empty download url returned from signer")`.
- `(*Client) GetGroupImage(host, groupID, fileID string) (*GetGroupImageResponse, error)`
  - Calls `fetchPresignedURL` for a temporary URL.
  - `downloadClient.R().SetDoNotParseResponse(true).Get(signedURL)` so the
    `RawBody()` is returned as an `io.ReadCloser`.
  - Populates `ContentType` (default `application/octet-stream` when empty) and
    `ContentLength`.

### 2.2 `pkg/model` change

Add a method to `pkg/model/user.go`:

```go
// DisplayName renders the user's display label for Drive ownership metadata.
func (u *User) DisplayName() string {
    switch {
    case u == nil:
        return ""
    case u.EngName == "" || u.ChineseName == "":
        return u.Account
    case u.EngName == u.ChineseName:
        return u.EngName
    default:
        return u.EngName + " " + u.ChineseName
    }
}
```

> This intentionally differs from `displayfmt.CombineWithFallback` (which
> returns the non-empty side); the task specifies returning `account` when
> **either** name is empty.

### 2.3 `upload-service/`

Files: `main.go`, `routes.go`, `middleware.go`, `handler.go`, `store.go`,
`store_mongo.go`, `handler_test.go`, `middleware_test.go`,
`mock_store_test.go` (generated), `integration_test.go`, and `deploy/`.

**`main.go` — config & wiring**

```go
type config struct {
    Port    string `env:"PORT" envDefault:"8080"`
    DevMode bool   `env:"DEV_MODE" envDefault:"false"`
    SiteID  string `env:"SITE_ID,required"`

    MongoURI string `env:"MONGO_URI,required"`
    MongoDB  string `env:"MONGO_DB" envDefault:"chat"`

    MaxFiles int `env:"MAX_FILES" envDefault:"10"`

    // OIDC (required when DEV_MODE=false), mirrors auth-service.
    OIDCIssuerURL string   `env:"OIDC_ISSUER_URL"`
    OIDCAudiences []string `env:"OIDC_AUDIENCES" envSeparator:","`
    TLSSkipVerify bool     `env:"TLS_SKIP_VERIFY" envDefault:"false"`

    Drive drive.Config `envPrefix:"DRIVE_"`
}
```

Startup: parse config → `cfg.Drive.LoadBaseURLs()` → `mongoutil.Connect` →
`otelutil.InitTracer` → build `drive.NewClient` → build `TokenValidator`
(`pkg/oidc.Validator`, or a dev pass-through when `DEV_MODE=true`) → build
Mongo store → build handler → Gin engine with middleware → `http.Server` with
read/write timeouts → graceful shutdown via `pkg/shutdown.Wait`
(`srv.Shutdown` → Mongo disconnect). Mongo collections: `subscriptions`,
`rooms`.

**`routes.go`**

```go
func registerRoutes(r *gin.Engine, h *Handler, v TokenValidator, devMode bool) {
    r.GET("/healthz", h.HandleHealth)
    api := r.Group("/api/v4")
    api.Use(otelMiddleware())          // small in-repo span-per-request
    api.Use(authMiddleware(v, devMode)) // validates ssoToken header, sets user in ctx
    api.POST("/rooms/:roomId/upload/protected-images", h.HandleUploadProtectedImages)
    api.GET("/rooms/:roomId/protected-image/:fileId", h.HandleDownloadProtectedImage)
}
```

The repo-wide `requestIDMiddleware` and `accessLogMiddleware` (copied from
auth-service) are applied at the engine level.

**`middleware.go` — auth + tracing**

```go
type TokenValidator interface {
    Validate(ctx context.Context, rawToken string) (oidc.Claims, error)
}
```

`authMiddleware`:
- Reads the TSSO token from the **`ssoToken`** header for **both** endpoints.
  Rationale: auth-service validates the same SSO token but receives it as the
  `ssoToken` JSON body field; here the bodies can't carry it (upload is
  `multipart/form-data`, download is a bodyless `GET`), so the token travels in
  a header. The header key is named `ssoToken` to match auth-service's field
  name. Validated via `pkg/oidc.Validator`. `X-User-Id` is read and logged but
  the validated token is the authoritative identity source.
- Empty token → 401. `Validate` error → 401 (expired → a distinct message,
  matching auth-service).
- On success, builds `AuthenticatedUser` from claims:
  - `User.Account` ← `claims.PreferredUsername` (fallback `claims.Name`),
  - engName/chineseName/employeeId ← parsed from `claims.Description` (reusing
    the auth-service `parseDescription` semantics),
  - `Email` ← `claims.Email`.
- Stores it in the gin context under a typed key; `userFromContext(c)` retrieves
  it. In `DevMode` the middleware accepts the token value as the account and
  synthesizes a dev user/email (mirrors `auth-service` dev mode).

```go
type AuthenticatedUser struct {
    model.User
    Email string
}
```

`otelMiddleware`: a thin handler that starts a span (`otel.Tracer(...).Start`)
named after the route, propagating `c.Request.Context()`, and ends it on
completion. No new dependency.

**`store.go` / `store_mongo.go`**

```go
type Store interface {
    IsMember(ctx context.Context, roomID, account string) (bool, error)
    GetRoom(ctx context.Context, roomID string) (*model.Room, error)
}
```

- `IsMember` — projected `FindOne` (`{_id:1}`) on `subscriptions` with filter
  `{ "roomId": roomID, "u.account": account }`, returns `true` unless
  `mongo.ErrNoDocuments`. (Lighter than `CountDocuments`, which runs an
  aggregation.) The supporting unique `(roomId, u.account)` index is owned by
  room-service.
- `GetRoom` — `FindOne` on `rooms` by `_id`; returns
  `(nil, ErrRoomNotFound)` (sentinel, wrapping `mongo.ErrNoDocuments`) when
  absent. `//go:generate mockgen` directive present; mock in
  `mock_store_test.go`.

**`handler.go`**

Handler struct holds `store Store`, `drive driveClient`, `maxFiles int`. The
Drive dependency is a narrow consumer-side interface so tests can fake it:

```go
type driveClient interface {
    UploadGroupImages(userID, username, email, groupID, origin string, files []drive.MultipartFile) ([]drive.UploadGroupImageResponse, error)
    GetGroupImage(host, groupID, fileID string) (*drive.GetGroupImageResponse, error)
    GetBaseURLFromRoomOrigin(origin string) string
}
```

*Upload* — `HandleUploadProtectedImages` (errcode codes per §3 table):
1. Parse `roomId` from path; missing → `errcode.BadRequest` (**400**).
2. `user := userFromContext(c)`; missing → `errcode.Internal` (**500**).
3. `email := user.Email`; empty → `errcode.Internal("the user has no email
   provided")` (**500**).
4. `requireMembership(ctx, c, roomId, user.Account)`: store error →
   `fmt.Errorf(…)` → **500** `internal`; not a member →
   `errcode.Forbidden(…, WithReason(RoomNotMember))` (**403**).
5. `store.GetRoom(ctx, roomId)`; `ErrRoomNotFound` → `errcode.NotFound("room
   not found")` (**404**); other error → `fmt.Errorf(…)` → **500** `internal`.
6. Parse multipart form (`c.MultipartForm()` / `c.Request.ParseMultipartForm`);
   non-multipart → `errcode.BadRequest` (**400**). File count over `maxFiles`
   → `errcode.BadRequest("too many files")` (**400**).
7. Per-file preprocessing over `form.File["images"]`, returning
   `(results []UploadProtectedImageResponse, fileHeaders []MultipartFile)`:
   - size > `UploadImageMaxSizeBytes` → append `{Name, Status:"failure",
     Error:"file size exceeds limit"}`, continue.
   - extension not in `AllowedImageFileTypes` → append `{Name, Status:"failure",
     Error:"file has an invalid file type"}`, continue.
   - `fh.Open()` fails → append `{Name, Status:"failure",
     Error:"failed to open file"}`, continue.
   - else append `{File, Filename}` to `fileHeaders`.
   Opened handles are registered for deferred close.
8. **Early exit:** if `len(fileHeaders) == 0`, return **200** immediately with
   the pre-check `results` (no Drive call).
9. `drive.UploadGroupImages(user.Account, user.DisplayName(), email, roomId,
   room.SiteID, fileHeaders)`; transport error → `fmt.Errorf(…)` → **500**
   `internal`.
10. Convert each Drive response into `UploadProtectedImageResponse`. When
    `Status == "Success"`, build
    `relativePath = fmt.Sprintf("api/v4/rooms/%s/protected-image/%s?drive_host=%s",
    resp.File.GroupID, resp.File.FileID, drive.GetBaseURLFromRoomOrigin(room.SiteID))`.
11. Merge pre-check `results` + converted upload results; return **200 OK**
    `{"results":[...]}` (both successes and failures in one body).

*Download* — `HandleDownloadProtectedImage` (errcode codes per §3 table):
1. Parse `roomId`/`fileId` from path; missing → `errcode.BadRequest` (**400**).
2. `drive_host` from query (`c.Query("drive_host")`); missing →
   `errcode.BadRequest("drive_host is required")` (**400**).
3. `user := userFromContext(c)`; missing → `errcode.Internal` (**500**).
4. `requireMembership(ctx, c, roomId, user.Account)`: not a member →
   `errcode.Forbidden(…, WithReason(RoomNotMember))` (**403**).
5. `drive.GetGroupImage(driveHost, roomId, fileId)`; any error (presign
   network/empty-URL, or download non-2xx) →
   `errcode.Unavailable("failed to retrieve image", WithCause(err))` (**503**).
6. Stream: `defer resp.Reader.Close()`; set `Content-Type`
   (`application/octet-stream` when empty) and `Content-Length` when known;
   `io.Copy(c.Writer, resp.Reader)`. **200 OK**, raw binary — no JSON, no
   buffering.

## 3. Error handling

> **Updated post-review:** the original design used a bespoke
> `{ errorType, error }` envelope + `abortError` helper. The service now adopts
> the repo-standard centralized error model (`pkg/errcode` +
> `pkg/errcode/errhttp`), identical to auth-service. Wire shape is
> `{ error, code, reason?, metadata? }` (see `docs/client-api.md` §6).

Handlers build a logging context once, then write via the adapter:

```go
ctx := errcode.WithLogValues(c.Request.Context(), "request_id", c.GetString("request_id"))
errhttp.Write(ctx, c, errcode.NotFound("room not found"))   // typed client error
errhttp.Write(ctx, c, fmt.Errorf("get room: %w", err))      // infra → collapses to internal
```

`errhttp.Write` runs `errcode.Classify`, which logs the failure **once** at a
category-aware level (ERROR for `internal`/`unavailable`, INFO for 4xx) and
writes the envelope with the code's HTTP status. Handlers therefore never `slog`
and write the same error. Membership 403s reuse the shared
`errcode.RoomNotMember` reason; auth 401s reuse `errcode.AuthTokenExpired` /
`AuthInvalidToken` / `AuthMissingFields`. The auth middleware calls `c.Abort()`
after `errhttp.Write` (the adapter writes but does not abort the chain).

**Upload — `POST /api/v4/rooms/:roomId/upload/protected-images`**

| Status | `code` | `reason` | Condition |
|--------|--------|----------|-----------|
| 400 | `bad_request` | — | Missing `roomId`; not multipart; or too many files. |
| 401 | `unauthenticated` | `sso_token_expired` / `invalid_sso_token` / `missing_fields` | Missing/invalid/expired `ssoToken`. |
| 403 | `forbidden` | `not_room_member` | Caller is not a room member. |
| 404 | `not_found` | — | Room does not exist. |
| 500 | `internal` | — | User missing in ctx, no email on the account, or a Drive/store fault. |

**Download — `GET /api/v4/rooms/:roomId/protected-image/:fileId`**

| Status | `code` | `reason` | Condition |
|--------|--------|----------|-----------|
| 400 | `bad_request` | — | Missing `roomId`, `fileId`, or `drive_host`. |
| 401 | `unauthenticated` | `sso_token_expired` / `invalid_sso_token` / `missing_fields` | Missing/invalid/expired `ssoToken`. |
| 403 | `forbidden` | `not_room_member` | Caller is not a room member. |
| 500 | `internal` | — | User missing in context. |
| 503 | `unavailable` | — | Drive signer/download failure. |

General:
- Infra failures wrapped with `fmt.Errorf("…: %w", err)` and handed to
  `errhttp.Write` — they collapse to `internal`, and the cause is logged
  server-side only, never sent to the client.
- Sentinel `ErrRoomNotFound` compared via `errors.Is`; `mongo.ErrNoDocuments`
  handled explicitly in the store.
- Request-ID correlation is attached via `errcode.WithLogValues`; never logs
  token values or file bodies.

## 4. Configuration (env)

`PORT`, `DEV_MODE`, `SITE_ID`, `MONGO_URI`, `MONGO_DB`, `MAX_IMAGE_SIZE_BYTES`
(default `26214400` = 25 MiB), `OIDC_ISSUER_URL`, `OIDC_AUDIENCES`,
`TLS_SKIP_VERIFY`, and Drive vars under prefix `DRIVE_`: `DRIVE_URL`,
`DRIVE_API_TOKEN`, `DRIVE_BASE_URL_CONFIG_PATH` (default
`etc/config/baseurls.json`).

## 5. Testing (TDD, Red → Green → Refactor)

- `pkg/model/user_test.go` — table-driven `DisplayName()` cases (both empty,
  only-eng, only-chinese, equal, distinct, account fallback).
- `pkg/drive` unit tests using `httptest.Server`:
  - `LoadBaseURLs` (valid, missing file, invalid JSON → empty map + warning),
  - `GetBaseURLFromRoomOrigin` (hit, miss → fallback),
  - `UploadGroupImages` (indexed fields, headers, parsed response, transport
    error),
  - `fetchPresignedURL` (success, network error, error status, empty URL),
  - `GetGroupImage` (success stream + content-type default, presign failure,
    download non-2xx),
  - `ImagesForm` add/count/clear/files.
- `upload-service/handler_test.go` with mocked `Store` + fake `driveClient` +
  injected `TokenValidator`. Upload branches: missing roomId, missing user, no
  email, not a member, store error, room not found, non-multipart, too many
  files, each per-file rejection (size/ext/open), all-rejected early exit, mixed
  success+failure merge & relativePath shape. Download branches: missing
  params, missing user, not a member, drive error → 503, success stream (body +
  content-type). `middleware_test.go`: missing token (401), invalid token
  (401), expired token, valid token populates context, dev-mode path.
- ≥ 80 % coverage; meaningful error-path coverage.

## 6. Deliverables / deploy

- `upload-service/deploy/Dockerfile` (multi-stage `golang:1.25.x-alpine` →
  `alpine:3.21`, build context repo root), `deploy/docker-compose.yml`
  (service + Mongo + a mock Drive, `BOOTSTRAP_STREAMS` n/a), and
  `deploy/azure-pipelines.yml`.
- `docs/client-api.md` updated with the two new client-facing endpoints
  (request/headers, multipart form field `images`, response schema, status
  codes, and the `relativePath` contract linking upload → download).

## 7. Decisions made (defaulted)

1. TSSO token is read from the **`ssoToken`** header for both endpoints
   (header key named to match auth-service's `ssoToken` JSON body field; bodies
   here can't carry it — multipart upload / bodyless GET); validated via
   `pkg/oidc.Validator`.
2. New package location is `pkg/drive`.
3. ~~`MAX_FILES` defaults to **10**.~~ **Superseded** (single-image API): the
   per-image ceiling is `MAX_IMAGE_SIZE_BYTES` (default `26214400` = 25 MiB).
4. Drive `groupId == roomId`; Drive `origin == room.SiteID`.
5. ~~Any Drive download failure maps to **502 Bad Gateway**.~~ **Superseded** (errcode adoption): errcode has no 502, so Drive failures map to **503 `unavailable`**, and room-not-found maps to **404 `not_found`** (semantic statuses).

## 8. Out of scope

- No NATS/JetStream integration.
- Public (non-protected) image flow — `UploadImageResponse` type is included for
  parity only.
