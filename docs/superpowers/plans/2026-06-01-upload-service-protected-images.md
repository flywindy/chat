# upload-service Protected-Image Upload/Download Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a new `upload-service` HTTP service exposing authenticated, room-scoped protected-image **upload** (`POST /api/v4/rooms/:roomId/upload/protected-images`) and **download** (`GET /api/v4/rooms/:roomId/protected-image/:fileId`) endpoints that proxy to an internal Drive, plus a reusable `pkg/drive` client.

**Architecture:** Gin HTTP service (mirrors `auth-service`), HTTP-in + MongoDB + Drive-HTTP-out, **no NATS/JetStream**. An auth middleware validates the `ssoToken` header via `pkg/oidc.Validator` and puts an `AuthenticatedUser` in the Gin context. A new `pkg/drive` package wraps the Drive API with Resty. Membership/room lookups go through a service-owned Mongo `Store`.

**Tech Stack:** Go 1.25, Gin, Resty (via `pkg/restyutil`), MongoDB driver v2 (via `pkg/mongoutil`), `pkg/oidc`, `pkg/otelutil`, `caarlos0/env`, `go.uber.org/mock` (mockgen), `stretchr/testify`.

**Module path:** `github.com/hmchangw/chat`. Always use `make` targets (never raw `go`).

---

## Post-review updates (applied after execution)

The tasks below were executed as written, then revised during PR review
(PR #263). Where this addendum conflicts with a task body, this addendum wins;
later updates supersede earlier ones.

### Update 3 — revert single-image back to batch

The Update 2 single-image upload was reverted to the **batch** API (partial
success), keeping the `/api/v1` move and the configurable size:

- `POST /api/v1/rooms/:roomId/upload/images` (plural), form field `images`
  (repeatable), handler `HandleUploadImages`. Download endpoint unchanged.
- `200` body is `{ "results": [ { name, status, error?, relativePath? } ] }`;
  per-file size/type/open rejections are `failure` entries (not request errors);
  `relativePath` = `api/v1/rooms/{groupId}/image/{fileId}?drive_host=…`.
- Drive client back to `UploadGroupImages([]MultipartFile) ([]resp, error)`
  (bulk); `preprocessFiles` restored.
- Both knobs apply: `MAX_FILES` (count, default 10, → 400 `too many files`) and
  `MAX_IMAGE_SIZE_BYTES` (per-image bytes, default 25 MiB). `NewHandler(store,
  drive, maxFiles, maxImageSize)`.

### Update 2 — single-image API, `/api/v1`, configurable size

- **Endpoints** moved to `/api/v1`: `POST /api/v1/rooms/:roomId/upload/image`
  and `GET /api/v1/rooms/:roomId/image/:fileId`. `relativePath` is now
  `api/v1/rooms/{groupId}/image/{fileId}?drive_host=…`.
- **Single-image upload** replaces the batch flow: one file under the `image`
  form field; validation failures (missing file / size / type) return a `4xx`
  errcode rather than a per-file `results` array; success returns
  `{ "name", "relativePath" }` (200). `preprocessFiles` was removed.
- **Drive client**: `UploadGroupImages([]MultipartFile) ([]resp,…)` →
  `UploadGroupImage(MultipartFile) (*resp,…)` (single file via the bulk
  endpoint; returns the lone result, erroring on an empty result).
- **Handlers renamed**: `HandleUploadProtectedImages`→`HandleUploadImage`,
  `HandleDownloadProtectedImage`→`HandleDownloadImage`.
- **Configurable max size**: `drive.UploadImageMaxSizeBytes` const removed; the
  ceiling is now `MAX_IMAGE_SIZE_BYTES` (env, default `26214400` = 25 MiB),
  passed into `NewHandler` and checked in the handler. `MAX_FILES` was removed.
- Tests rewritten accordingly (`TestUpload_Success_200`,
  `TestUpload_MissingImageFile_400`, `…_FileTooLarge_400`,
  `…_InvalidFileType_400`, `…_DriveStatusFailure_500`; batch-specific tests
  dropped). drive tests: `TestClient_UploadGroupImage` (+ `_EmptyResult`).

### Update 1 — errcode adoption & review fixes

Where this addendum conflicts with a task body, this addendum wins:

- **Error model — adopted `pkg/errcode` + `pkg/errcode/errhttp`.** Task 7's
  bespoke `errorResponse{errorType,error}` envelope + `abortError` helper was
  **removed** (`errors.go`/`errors_test.go` deleted). Handlers and the auth
  middleware now build `ctx := errcode.WithLogValues(c.Request.Context(),
  "request_id", …)` and call `errhttp.Write(ctx, c, errcode.X(…))` /
  `errhttp.Write(ctx, c, fmt.Errorf("…: %w", err))`. `Classify` logs once, so
  the manual `slog` error lines were dropped; the middleware adds `c.Abort()`
  after each write. Wire shape is now the standard `{ error, code, reason? }`.
- **Status changes** (from the errcode taxonomy): room-not-found **400 → 404**
  (`not_found`); Drive signer/download failure **502 → 503** (`unavailable`,
  errcode has no 502). Membership 403 reuses `errcode.RoomNotMember`; auth 401s
  reuse `errcode.AuthTokenExpired`/`AuthInvalidToken`/`AuthMissingFields`. Tests
  `TestUpload_RoomNotFound_400`→`_404` and `TestDownload_DriveError_502`→`_503`.
- **`IsMember`** uses a projected `FindOne` (`{_id:1}`) existence check rather
  than `CountDocuments` (lighter — no aggregation). The supporting unique
  `(roomId, u.account)` index is owned by room-service, so upload-service does
  **not** add an `EnsureIndexes`.
- **Membership dedup** — the duplicated check is factored into a
  `Handler.requireMembership(ctx, c, roomID, account) bool` helper used by both
  handlers.
- **`preprocessFiles`** returns `(results, fileHeaders)` (the redundant `opened`
  slice was dropped; the caller closes via `fileHeaders[i].File`).
- **TLS** — `drive.NewClient` sets `MinVersion: tls.VersionTLS13` alongside the
  (justified, `#nosec G402`) `InsecureSkipVerify` to satisfy the semgrep
  `missing-ssl-minversion` SAST gate.
- **Registration** — `docker-local/compose.services.yaml` includes
  `upload-service/deploy/docker-compose.yml`.

---

## File Structure

**`pkg/drive/`** (new package)
- `config.go` — `Config` + `LoadBaseURLs()`.
- `images_file.go` — constants, response structs, `File`, `ImagesFormData`.
- `uploader.go` — `MultipartFile`, `Client`, `NewClient`, `GetBaseURL`, `GetBaseURLFromRoomOrigin`, `UploadGroupImages`, `fetchPresignedURL`, `GetGroupImage`.
- `config_test.go`, `images_file_test.go`, `uploader_test.go`.

**`pkg/model/`** (modify)
- `user.go` — add `(*User).DisplayName()`.
- `user_test.go` — new, table-driven `DisplayName` tests.

**`upload-service/`** (new service, flat `package main`)
- `main.go` — config, wiring, startup, graceful shutdown.
- `routes.go` — route registration + `/api/v4` group.
- `middleware.go` — `requestIDMiddleware`, `accessLogMiddleware`, `otelMiddleware`, `authMiddleware`, `TokenValidator`, `AuthenticatedUser`, `userFromContext`, `parseDescription`.
- `errors.go` — `errorResponse`, `errorType` constants, status constants, `abortError`.
- `handler.go` — `Handler`, `driveClient` interface, `NewHandler`, `HandleUploadProtectedImages`, `HandleDownloadProtectedImage`, `HandleHealth`, `preprocessFiles`.
- `store.go` — `Store` interface, `ErrRoomNotFound`, `//go:generate` directive.
- `store_mongo.go` — `mongoStore` implementation.
- `mock_store_test.go` — generated (never hand-edited).
- `errors_test.go`, `middleware_test.go`, `handler_test.go` — unit tests.
- `integration_test.go` — `//go:build integration` store tests with testcontainers.
- `deploy/Dockerfile`, `deploy/docker-compose.yml`, `deploy/azure-pipelines.yml`.

**`docs/client-api.md`** (modify) — document both endpoints.

---

## Task 1: `model.User.DisplayName()`

**Files:**
- Modify: `pkg/model/user.go`
- Test: `pkg/model/user_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `pkg/model/user_test.go`:

```go
package model

import "testing"

func TestUser_DisplayName(t *testing.T) {
	tests := []struct {
		name string
		user *User
		want string
	}{
		{"nil user", nil, ""},
		{"both names empty -> account", &User{Account: "alice", EngName: "", ChineseName: ""}, "alice"},
		{"eng empty -> account", &User{Account: "alice", EngName: "", ChineseName: "陳"}, "alice"},
		{"chinese empty -> account", &User{Account: "alice", EngName: "Alice", ChineseName: ""}, "alice"},
		{"equal names -> eng", &User{Account: "alice", EngName: "Same", ChineseName: "Same"}, "Same"},
		{"distinct names -> joined", &User{Account: "alice", EngName: "Alice", ChineseName: "陳"}, "Alice 陳"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.DisplayName(); got != tc.want {
				t.Fatalf("DisplayName() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/model`
Expected: FAIL — `tc.user.DisplayName undefined`.

- [ ] **Step 3: Add the method**

Append to `pkg/model/user.go` (after the `User` struct):

```go
// DisplayName renders the user's display label for Drive ownership metadata:
// the account when either name is missing, the English name when both names are
// identical, otherwise "<engName> <chineseName>".
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

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/model`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/model/user.go pkg/model/user_test.go
git commit -m "feat(model): add User.DisplayName for drive ownership label"
```

---

## Task 2: `pkg/drive` config + `LoadBaseURLs`

**Files:**
- Create: `pkg/drive/config.go`
- Test: `pkg/drive/config_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/drive/config_test.go`:

```go
package drive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfig_LoadBaseURLs_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseurls.json")
	if err := os.WriteFile(path, []byte(`{"site-a":"https://a.example.com","site-b":"https://b.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{BaseURLConfigPath: path}
	c.LoadBaseURLs()
	if c.BaseURLMap["site-a"] != "https://a.example.com" {
		t.Fatalf("got %q", c.BaseURLMap["site-a"])
	}
	if c.BaseURLMap["site-b"] != "https://b.example.com" {
		t.Fatalf("got %q", c.BaseURLMap["site-b"])
	}
}

func TestConfig_LoadBaseURLs_MissingFile(t *testing.T) {
	c := &Config{BaseURLConfigPath: filepath.Join(t.TempDir(), "does-not-exist.json")}
	c.LoadBaseURLs()
	if c.BaseURLMap == nil {
		t.Fatal("expected empty (non-nil) map")
	}
	if len(c.BaseURLMap) != 0 {
		t.Fatalf("expected empty map, got %v", c.BaseURLMap)
	}
}

func TestConfig_LoadBaseURLs_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{BaseURLConfigPath: path}
	c.LoadBaseURLs()
	if c.BaseURLMap == nil || len(c.BaseURLMap) != 0 {
		t.Fatalf("expected empty map on invalid json, got %v", c.BaseURLMap)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/drive`
Expected: FAIL — package/type undefined.

- [ ] **Step 3: Implement `config.go`**

Create `pkg/drive/config.go`:

```go
// Package drive is a client for the internal Drive file-storage API.
package drive

import (
	"encoding/json"
	"log/slog"
	"os"
)

// Config holds Drive connection settings parsed from environment variables.
type Config struct {
	URL               string `env:"URL"`
	Token             string `env:"API_TOKEN"`
	BaseURLConfigPath string `env:"BASE_URL_CONFIG_PATH" envDefault:"etc/config/baseurls.json"`
	// BaseURLMap maps a room-origin siteID to a Drive base URL. Populated by LoadBaseURLs.
	BaseURLMap map[string]string
}

// LoadBaseURLs reads and JSON-parses the file at BaseURLConfigPath into
// BaseURLMap. On a missing file or invalid JSON it logs a warning and falls
// back to an empty map so the service still starts.
func (c *Config) LoadBaseURLs() {
	// #nosec G304 -- path is operator-supplied configuration, not user input.
	data, err := os.ReadFile(c.BaseURLConfigPath)
	if err != nil {
		slog.Warn("drive: could not read base URL config; using empty map",
			"path", c.BaseURLConfigPath, "error", err)
		c.BaseURLMap = map[string]string{}
		return
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("drive: invalid base URL config JSON; using empty map",
			"path", c.BaseURLConfigPath, "error", err)
		c.BaseURLMap = map[string]string{}
		return
	}
	c.BaseURLMap = m
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/drive`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/drive/config.go pkg/drive/config_test.go
git commit -m "feat(drive): add Config and LoadBaseURLs"
```

---

## Task 3: `pkg/drive` types + `ImagesFormData`

**Files:**
- Create: `pkg/drive/images_file.go`
- Test: `pkg/drive/images_file_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/drive/images_file_test.go`:

```go
package drive

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImagesFormData_AddCountClearFiles(t *testing.T) {
	f := NewImagesForm()
	if f.FileCount() != 0 {
		t.Fatalf("new form should be empty, got %d", f.FileCount())
	}
	f.AddFile("a.png", "image/png", strings.NewReader("aaa"))
	f.AddFile("b.jpg", "image/jpeg", strings.NewReader("bbb"))
	if f.FileCount() != 2 {
		t.Fatalf("want 2 files, got %d", f.FileCount())
	}
	if got := f.Files(); got[0].Filename != "a.png" || got[1].ContentType != "image/jpeg" {
		t.Fatalf("unexpected files slice: %+v", got)
	}
	f.ClearFiles()
	if f.FileCount() != 0 {
		t.Fatalf("ClearFiles should empty the slice, got %d", f.FileCount())
	}
}

func TestUploadGroupImageResponse_JSONTags(t *testing.T) {
	// File must marshal under "object"; GroupImageObject fields under objectId/groupId/fileName.
	in := UploadGroupImageResponse{
		Status: "Success",
		File:   GroupImageObject{FileID: "f1", GroupID: "r1", Filename: "a.png"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"object":`, `"objectId":"f1"`, `"groupId":"r1"`, `"fileName":"a.png"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("marshaled JSON %s missing %s", s, want)
		}
	}
}

func TestAllowedImageFileTypes(t *testing.T) {
	for _, ext := range []string{".png", ".jpeg", ".jpg", ".heic"} {
		if !AllowedImageFileTypes[ext] {
			t.Fatalf("%s should be allowed", ext)
		}
	}
	if AllowedImageFileTypes[".exe"] {
		t.Fatal(".exe must not be allowed")
	}
	if UploadImageMaxSizeBytes != 25*1024*1024 {
		t.Fatalf("max size = %d", UploadImageMaxSizeBytes)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/drive`
Expected: FAIL — undefined types/functions.

- [ ] **Step 3: Implement `images_file.go`**

Create `pkg/drive/images_file.go`:

```go
package drive

import "io"

// UploadImageMaxSizeBytes is the per-file upload ceiling (25 MiB).
const UploadImageMaxSizeBytes int64 = 25 * 1024 * 1024

// AllowedImageFileTypes is the set of accepted lowercase file extensions.
var AllowedImageFileTypes = map[string]bool{
	".png":  true,
	".jpeg": true,
	".jpg":  true,
	".heic": true,
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

// GetGroupImageResponse carries a streamed download body plus metadata.
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

// File is one staged upload file.
type File struct {
	Reader      io.Reader
	Filename    string
	ContentType string
}

// ImagesFormData accumulates files staged for upload.
type ImagesFormData struct {
	files []File
}

// NewImagesForm returns an empty ImagesFormData.
func NewImagesForm() *ImagesFormData {
	return &ImagesFormData{files: []File{}}
}

// AddFile appends a file built from the provided filename, content type and reader.
func (f *ImagesFormData) AddFile(filename, contentType string, r io.Reader) {
	f.files = append(f.files, File{Reader: r, Filename: filename, ContentType: contentType})
}

// FileCount returns the number of staged files.
func (f *ImagesFormData) FileCount() int {
	return len(f.files)
}

// ClearFiles re-initializes the internal slice to empty.
func (f *ImagesFormData) ClearFiles() {
	f.files = []File{}
}

// Files returns the staged files.
func (f *ImagesFormData) Files() []File {
	return f.files
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/drive`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/drive/images_file.go pkg/drive/images_file_test.go
git commit -m "feat(drive): add image response types and ImagesFormData"
```

---

## Task 4: `pkg/drive` Client + base-URL routing

**Files:**
- Create: `pkg/drive/uploader.go`
- Test: `pkg/drive/uploader_test.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/drive/uploader_test.go`:

```go
package drive

import "testing"

func TestClient_GetBaseURLFromRoomOrigin(t *testing.T) {
	c := NewClient(&Config{
		URL:        "https://default.example.com",
		Token:      "tok",
		BaseURLMap: map[string]string{"site-a": "https://a.example.com"},
	})
	if got := c.GetBaseURL(); got != "https://default.example.com" {
		t.Fatalf("GetBaseURL = %q", got)
	}
	if got := c.GetBaseURLFromRoomOrigin("site-a"); got != "https://a.example.com" {
		t.Fatalf("known origin = %q", got)
	}
	if got := c.GetBaseURLFromRoomOrigin("unknown"); got != "https://default.example.com" {
		t.Fatalf("unknown origin should fall back to base, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/drive`
Expected: FAIL — `NewClient`/`Client` undefined.

- [ ] **Step 3: Implement `uploader.go` (client + routing only)**

Create `pkg/drive/uploader.go`:

```go
package drive

import (
	"crypto/tls"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/hmchangw/chat/pkg/restyutil"
)

// MultipartFile is an opened multipart file plus its name, ready to upload.
type MultipartFile struct {
	File     multipart.File
	Filename string
}

// Client talks to the internal Drive API.
type Client struct {
	uploadClient   *resty.Client
	downloadClient *resty.Client
	baseURLMap     map[string]string
	baseURL        string
	apiToken       string
}

// NewClient builds a Drive client. Both underlying Resty clients skip TLS
// verification (the Drive is reached over a private network); the download
// client uses a 5-minute timeout to allow large streamed bodies.
func NewClient(cfg *Config) *Client {
	// #nosec G402 -- internal Drive over a private network; TLS verification is intentionally skipped per deployment.
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	return &Client{
		uploadClient:   restyutil.New(cfg.URL, restyutil.WithTransport(insecure)),
		downloadClient: restyutil.New(cfg.URL, restyutil.WithTransport(insecure), restyutil.WithTimeout(5*time.Minute)),
		baseURLMap:     cfg.BaseURLMap,
		baseURL:        cfg.URL,
		apiToken:       cfg.Token,
	}
}

// GetBaseURL returns the default Drive base URL.
func (c *Client) GetBaseURL() string { return c.baseURL }

// GetBaseURLFromRoomOrigin returns the Drive base URL for a room-origin siteID,
// falling back to the default base URL when the origin is unknown.
func (c *Client) GetBaseURLFromRoomOrigin(origin string) string {
	if url, ok := c.baseURLMap[origin]; ok && url != "" {
		return url
	}
	return c.baseURL
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/drive`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/drive/uploader.go pkg/drive/uploader_test.go
git commit -m "feat(drive): add Client constructor and base-URL routing"
```

---

## Task 5: `pkg/drive` UploadGroupImages

**Files:**
- Modify: `pkg/drive/uploader.go`
- Test: `pkg/drive/uploader_test.go`

- [ ] **Step 1: Write the failing test**

Replace the import block of `pkg/drive/uploader_test.go` with:

```go
import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)
```

Then append:

```go
func TestClient_UploadGroupImages(t *testing.T) {
	var gotPath, gotToken, gotUserID, gotFileName, gotMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("api-token")
		_ = r.ParseMultipartForm(10 << 20)
		gotUserID = r.FormValue("userId")
		gotFileName = r.FormValue("files[0].fileName")
		gotMode = r.FormValue("files[0].mode")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"status":"Success","object":{"objectId":"f1","groupId":"r1","fileName":"a.png"}}]`)
	}))
	defer srv.Close()

	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	files := []MultipartFile{{File: fakeMultipart("aaa"), Filename: "a.png"}}
	resp, err := c.UploadGroupImages("alice", "Alice", "a@x.com", "r1", "site-x", files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp) != 1 || resp[0].Status != "Success" || resp[0].File.FileID != "f1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotPath != "/api/v1/groups/r1/files/bulk" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotToken != "tok" || gotUserID != "alice" || gotFileName != "a.png" || gotMode != "Normal" {
		t.Fatalf("token=%q userId=%q fileName=%q mode=%q", gotToken, gotUserID, gotFileName, gotMode)
	}
}

func TestClient_UploadGroupImages_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	_, err := c.UploadGroupImages("alice", "Alice", "a@x.com", "r1", "site-x",
		[]MultipartFile{{File: fakeMultipart("x"), Filename: "a.png"}})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

// fakeFile adapts a string to multipart.File for tests.
type fakeFile struct{ *strings.Reader }

func (fakeFile) Close() error                { return nil }
func fakeMultipart(s string) multipart.File  { return fakeFile{strings.NewReader(s)} }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/drive`
Expected: FAIL — `UploadGroupImages` undefined.

- [ ] **Step 3: Implement `UploadGroupImages`**

Append to `pkg/drive/uploader.go` (add imports `fmt`):

```go
// UploadGroupImages uploads files to a Drive group in one bulk multipart call.
// userID/username/email are sent as form fields; each file is attached with the
// indexed naming convention files[i].file / files[i].fileName / files[i].mode.
func (c *Client) UploadGroupImages(userID, username, email, groupID, origin string, files []MultipartFile) ([]UploadGroupImageResponse, error) {
	req := c.uploadClient.R().
		SetHeader("api-token", c.apiToken).
		SetFormData(map[string]string{
			"userId":   userID,
			"username": username,
			"email":    email,
		})
	for i, f := range files {
		field := fmt.Sprintf("files[%d].file", i)
		req.SetMultipartField(field, f.Filename, "application/octet-stream", f.File)
		req.SetFormData(map[string]string{
			fmt.Sprintf("files[%d].fileName", i): f.Filename,
			fmt.Sprintf("files[%d].mode", i):     "Normal",
		})
	}

	var result []UploadGroupImageResponse
	resp, err := req.
		SetResult(&result).
		SetPathParam("groupId", groupID).
		Post(fmt.Sprintf("%s/api/v1/groups/{groupId}/files/bulk", c.GetBaseURLFromRoomOrigin(origin)))
	if err != nil {
		return nil, fmt.Errorf("upload group images: %w", err)
	}
	if resp.IsError() {
		return nil, fmt.Errorf("drive bulk upload returned status %d", resp.StatusCode())
	}
	return result, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/drive`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/drive/uploader.go pkg/drive/uploader_test.go
git commit -m "feat(drive): add UploadGroupImages bulk indexed upload"
```

---

## Task 6: `pkg/drive` fetchPresignedURL + GetGroupImage

**Files:**
- Modify: `pkg/drive/uploader.go`
- Test: `pkg/drive/uploader_test.go`

- [ ] **Step 1: Write the failing test**

Append to `pkg/drive/uploader_test.go`:

```go
func TestClient_GetGroupImage_Success(t *testing.T) {
	mux := http.NewServeMux()
	// signer returns a presigned URL pointing back at /img on the same server.
	var base string
	mux.HandleFunc("/api/v1/groups/r1/files/f1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":"`+base+`/img"}`)
	})
	mux.HandleFunc("/img", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	img, err := c.GetGroupImage(srv.URL, "r1", "f1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer img.Reader.Close()
	if img.ContentType != "image/png" {
		t.Fatalf("content type = %q", img.ContentType)
	}
	body, _ := io.ReadAll(img.Reader)
	if string(body) != "PNGDATA" {
		t.Fatalf("body = %q", string(body))
	}
}

func TestClient_GetGroupImage_EmptyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":""}`)
	}))
	defer srv.Close()
	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	if _, err := c.GetGroupImage(srv.URL, "r1", "f1"); err == nil {
		t.Fatal("expected error on empty signer URL")
	}
}

func TestClient_GetGroupImage_SignerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}))
	defer srv.Close()
	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	if _, err := c.GetGroupImage(srv.URL, "r1", "f1"); err == nil {
		t.Fatal("expected error on signer 404")
	}
}

func TestClient_GetGroupImage_DownloadNotFound(t *testing.T) {
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/api/v1/groups/r1/files/f1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":"`+base+`/img"}`)
	})
	mux.HandleFunc("/img", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	c := NewClient(&Config{URL: srv.URL, Token: "tok"})
	_, err := c.GetGroupImage(srv.URL, "r1", "f1")
	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("expected image-not-found error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=pkg/drive`
Expected: FAIL — `GetGroupImage` undefined.

- [ ] **Step 3: Implement `fetchPresignedURL` + `GetGroupImage`**

Append to `pkg/drive/uploader.go`:

```go
// fetchPresignedURL asks the Drive signer for a temporary download URL.
func (c *Client) fetchPresignedURL(host, fileID, groupID string) (string, error) {
	type presignedURL struct {
		URL   string `json:"url"`
		Error string `json:"error,omitempty"`
	}
	var result presignedURL
	resp, err := c.downloadClient.R().
		SetHeader("api-token", c.apiToken).
		SetResult(&result).
		SetPathParam("groupId", groupID).
		SetPathParam("fileId", fileID).
		Get(fmt.Sprintf("%s/api/v1/groups/{groupId}/files/{fileId}", host))
	if err != nil {
		return "", fmt.Errorf("network error calling signer service: %w", err)
	}
	if resp.IsError() {
		return "", fmt.Errorf("signer service returned status %d: %s", resp.StatusCode(), result.Error)
	}
	if result.URL == "" {
		return "", fmt.Errorf("empty download url returned from signer")
	}
	return result.URL, nil
}

// GetGroupImage resolves a presigned URL then streams the image bytes. The
// returned Reader is the raw response body and must be closed by the caller.
func (c *Client) GetGroupImage(host, groupID, fileID string) (*GetGroupImageResponse, error) {
	signedURL, err := c.fetchPresignedURL(host, fileID, groupID)
	if err != nil {
		return nil, fmt.Errorf("fetch presigned url: %w", err)
	}
	resp, err := c.downloadClient.R().
		SetDoNotParseResponse(true).
		Get(signedURL)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	if resp.IsError() {
		defer resp.RawBody().Close()
		if resp.StatusCode() == http.StatusNotFound {
			return nil, fmt.Errorf("image not found")
		}
		return nil, fmt.Errorf("failed to fetch image from storage, status: %d", resp.StatusCode())
	}
	contentType := resp.Header().Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var contentLength int64
	if resp.RawResponse != nil {
		contentLength = resp.RawResponse.ContentLength
	}
	return &GetGroupImageResponse{
		Reader:        resp.RawBody(),
		ContentType:   contentType,
		ContentLength: contentLength,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=pkg/drive`
Expected: PASS.

- [ ] **Step 5: Lint + commit**

Run: `make lint` (expect clean; if gosec flags G402/G304 in CI, the `// #nosec` directives already cover them).

```bash
git add pkg/drive/uploader.go pkg/drive/uploader_test.go
git commit -m "feat(drive): add presigned-URL fetch and streamed image download"
```

---

## Task 7: upload-service error envelope

**Files:**
- Create: `upload-service/errors.go`
- Test: `upload-service/errors_test.go`

- [ ] **Step 1: Write the failing test**

Create `upload-service/errors_test.go`:

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAbortError_WritesEnvelopeAndAborts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)

	abortError(c, http.StatusBadRequest, errTypeInvalidPathParam, "roomId: required")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	var got errorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ErrorType != "error-invalid-path-param" || got.Error != "roomId: required" {
		t.Fatalf("envelope = %+v", got)
	}
	if !c.IsAborted() {
		t.Fatal("context must be aborted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=upload-service`
Expected: FAIL — undefined symbols (and the package has no other files yet).

- [ ] **Step 3: Implement `errors.go`**

Create `upload-service/errors.go`:

```go
package main

import "github.com/gin-gonic/gin"

// Per-file result status values for the upload response.
const (
	statusFailure      = "failure" // pre-check rejection
	driveStatusSuccess = "Success" // Drive's success marker
)

// errorType values returned to clients in the error envelope.
const (
	errTypeInvalidPathParam = "error-invalid-path-param"
	errTypeInvalidRoomID    = "error-invalid-room-id"
	errTypeInvalidFormParam = "error-invalid-form-param"
	errTypeUserNotInRoom    = "error-user-not-in-room"
	errTypeInvalidUser      = "error-invalid-user"
	errTypeInternal         = "error-internal"
)

// errorResponse is the typed error envelope all error responses use.
type errorResponse struct {
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// abortError writes the error envelope with the given status and aborts the chain.
func abortError(c *gin.Context, status int, errType, msg string) {
	c.AbortWithStatusJSON(status, errorResponse{ErrorType: errType, Error: msg})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add upload-service/errors.go upload-service/errors_test.go
git commit -m "feat(upload-service): add typed error envelope helper"
```

---

## Task 8: upload-service Store interface + Mongo impl + mocks

**Files:**
- Create: `upload-service/store.go`, `upload-service/store_mongo.go`, `upload-service/integration_test.go`
- Generated: `upload-service/mock_store_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `upload-service/integration_test.go`:

```go
//go:build integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/testutil"
)

func TestMain(m *testing.M) { testutil.RunTests(m) }

func TestMongoStore_IsMemberAndGetRoom(t *testing.T) {
	db := testutil.MongoDB(t, "uploadsvc")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := db.Collection("subscriptions").InsertOne(ctx, bson.M{
		"_id": "sub1", "roomId": "r1", "u": bson.M{"_id": "u1", "account": "alice"},
	})
	require.NoError(t, err)
	_, err = db.Collection("rooms").InsertOne(ctx, bson.M{"_id": "r1", "name": "Room 1", "siteId": "site-x"})
	require.NoError(t, err)

	s := NewMongoStore(db)

	member, err := s.IsMember(ctx, "r1", "alice")
	require.NoError(t, err)
	require.True(t, member)

	member, err = s.IsMember(ctx, "r1", "bob")
	require.NoError(t, err)
	require.False(t, member)

	room, err := s.GetRoom(ctx, "r1")
	require.NoError(t, err)
	require.Equal(t, "site-x", room.SiteID)

	_, err = s.GetRoom(ctx, "missing")
	require.True(t, errors.Is(err, ErrRoomNotFound))

	_ = model.Room{}
}
```

- [ ] **Step 2: Run integration test to verify it fails**

Run: `make test-integration SERVICE=upload-service`
Expected: FAIL — `NewMongoStore`/`Store`/`ErrRoomNotFound` undefined.

- [ ] **Step 3: Implement `store.go`**

Create `upload-service/store.go`:

```go
package main

import (
	"context"
	"errors"

	"github.com/hmchangw/chat/pkg/model"
)

// ErrRoomNotFound is returned by GetRoom when no room matches the given ID.
var ErrRoomNotFound = errors.New("room not found")

//go:generate mockgen -source=store.go -destination=mock_store_test.go -package=main

// Store is the subset of persistence the upload handlers need.
type Store interface {
	// IsMember reports whether account has a subscription to roomID.
	IsMember(ctx context.Context, roomID, account string) (bool, error)
	// GetRoom returns the room by ID, or ErrRoomNotFound (wrapped) when absent.
	GetRoom(ctx context.Context, roomID string) (*model.Room, error)
}
```

- [ ] **Step 4: Implement `store_mongo.go`**

Create `upload-service/store_mongo.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/hmchangw/chat/pkg/model"
)

type mongoStore struct {
	subscriptions *mongo.Collection
	rooms         *mongo.Collection
}

// NewMongoStore returns a Store backed by the subscriptions and rooms collections.
func NewMongoStore(db *mongo.Database) *mongoStore {
	return &mongoStore{
		subscriptions: db.Collection("subscriptions"),
		rooms:         db.Collection("rooms"),
	}
}

func (s *mongoStore) IsMember(ctx context.Context, roomID, account string) (bool, error) {
	count, err := s.subscriptions.CountDocuments(ctx, bson.M{"roomId": roomID, "u.account": account})
	if err != nil {
		return false, fmt.Errorf("count subscription for room %s: %w", roomID, err)
	}
	return count > 0, nil
}

func (s *mongoStore) GetRoom(ctx context.Context, roomID string) (*model.Room, error) {
	var room model.Room
	if err := s.rooms.FindOne(ctx, bson.M{"_id": roomID}).Decode(&room); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("get room %s: %w", roomID, ErrRoomNotFound)
		}
		return nil, fmt.Errorf("get room %s: %w", roomID, err)
	}
	return &room, nil
}
```

- [ ] **Step 5: Generate the mock**

Run: `make generate SERVICE=upload-service`
Expected: creates `upload-service/mock_store_test.go` (a `MockStore` with `IsMember`/`GetRoom`).

- [ ] **Step 6: Run integration test to verify it passes**

Run: `make test-integration SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add upload-service/store.go upload-service/store_mongo.go upload-service/mock_store_test.go upload-service/integration_test.go
git commit -m "feat(upload-service): add Store interface, Mongo impl, mocks"
```

---

## Task 9: upload-service middleware (request-id, access-log, otel, auth)

**Files:**
- Create: `upload-service/middleware.go`
- Test: `upload-service/middleware_test.go`

- [ ] **Step 1: Write the failing test**

Create `upload-service/middleware_test.go`:

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
)

type fakeValidator struct {
	claims pkgoidc.Claims
	err    error
}

func (f fakeValidator) Validate(_ context.Context, _ string) (pkgoidc.Claims, error) {
	return f.claims, f.err
}

func runAuth(t *testing.T, v TokenValidator, devMode bool, token string) (*httptest.ResponseRecorder, *AuthenticatedUser) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(authMiddleware(v, devMode))
	var captured *AuthenticatedUser
	r.GET("/x", func(c *gin.Context) {
		if u, ok := userFromContext(c); ok {
			captured = u
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if token != "" {
		req.Header.Set("ssoToken", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, captured
}

func TestAuthMiddleware_MissingToken_401(t *testing.T) {
	w, _ := runAuth(t, fakeValidator{}, false, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_InvalidToken_401(t *testing.T) {
	w, _ := runAuth(t, fakeValidator{err: errors.New("bad")}, false, "tok")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_ExpiredToken_401(t *testing.T) {
	w, _ := runAuth(t, fakeValidator{err: pkgoidc.ErrTokenExpired}, false, "tok")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthMiddleware_ValidToken_PopulatesUser(t *testing.T) {
	v := fakeValidator{claims: pkgoidc.Claims{
		PreferredUsername: "alice",
		Email:             "alice@x.com",
		Description:       "E123, Alice, 陳大文",
	}}
	w, u := runAuth(t, v, false, "tok")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, u)
	assert.Equal(t, "alice", u.Account)
	assert.Equal(t, "alice@x.com", u.Email)
	assert.Equal(t, "Alice", u.EngName)
	assert.Equal(t, "陳大文", u.ChineseName)
	assert.Equal(t, "Alice 陳大文", u.DisplayName())
}

func TestAuthMiddleware_DevMode_SynthesizesUser(t *testing.T) {
	w, u := runAuth(t, nil, true, "alice")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, u)
	assert.Equal(t, "alice", u.Account)
	assert.Equal(t, "alice@dev.local", u.Email)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=upload-service`
Expected: FAIL — `authMiddleware`/`TokenValidator`/`AuthenticatedUser`/`userFromContext` undefined.

- [ ] **Step 3: Implement `middleware.go`**

Create `upload-service/middleware.go`:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"

	"github.com/hmchangw/chat/pkg/idgen"
	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/natsutil"
	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
)

const ctxUserKey = "auth_user"

// TokenValidator validates a TSSO token and returns OIDC claims.
// Satisfied by *pkg/oidc.Validator.
type TokenValidator interface {
	Validate(ctx context.Context, rawToken string) (pkgoidc.Claims, error)
}

// AuthenticatedUser is the identity resolved from a validated token.
type AuthenticatedUser struct {
	model.User
	Email string
}

// userFromContext returns the AuthenticatedUser set by authMiddleware.
func userFromContext(c *gin.Context) (*AuthenticatedUser, bool) {
	v, ok := c.Get(ctxUserKey)
	if !ok {
		return nil, false
	}
	u, ok := v.(*AuthenticatedUser)
	return u, ok
}

// requestIDMiddleware extracts or mints the request correlation ID.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(natsutil.RequestIDHeader)
		if !idgen.IsValidUUID(id) {
			id = idgen.GenerateRequestID()
		}
		c.Set("request_id", id)
		c.Request = c.Request.WithContext(natsutil.WithRequestID(c.Request.Context(), id))
		c.Header(natsutil.RequestIDHeader, id)
		c.Next()
	}
}

// accessLogMiddleware logs one structured line per request.
func accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"request_id", c.GetString("request_id"),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}

// otelMiddleware starts a span per request using the already-vendored otel API.
func otelMiddleware() gin.HandlerFunc {
	tracer := otel.Tracer("upload-service")
	return func(c *gin.Context) {
		name := c.FullPath()
		if name == "" {
			name = c.Request.URL.Path
		}
		ctx, span := tracer.Start(c.Request.Context(), name)
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// authMiddleware validates the ssoToken header and stores an AuthenticatedUser
// in the Gin context. In dev mode the header value is treated as the account.
func authMiddleware(v TokenValidator, devMode bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("ssoToken")
		if token == "" {
			abortError(c, http.StatusUnauthorized, errTypeInvalidUser, "missing ssoToken")
			return
		}

		var user AuthenticatedUser
		if devMode {
			user = AuthenticatedUser{
				User:  model.User{Account: token, EngName: token},
				Email: token + "@dev.local",
			}
		} else {
			claims, err := v.Validate(c.Request.Context(), token)
			if err != nil {
				if errors.Is(err, pkgoidc.ErrTokenExpired) {
					abortError(c, http.StatusUnauthorized, errTypeInvalidUser, "sso token has expired")
					return
				}
				slog.Warn("sso token validation failed", "error", err)
				abortError(c, http.StatusUnauthorized, errTypeInvalidUser, "invalid sso token")
				return
			}
			account := claims.PreferredUsername
			if account == "" {
				account = claims.Name
			}
			_, engName, chineseName := parseDescription(claims.Description)
			user = AuthenticatedUser{
				User: model.User{
					Account:     account,
					EngName:     engName,
					ChineseName: chineseName,
				},
				Email: claims.Email,
			}
		}

		c.Set(ctxUserKey, &user)
		c.Next()
	}
}

// parseDescription splits "employeeId, engName, chineseName" into its parts.
func parseDescription(desc string) (employeeID, engName, chineseName string) {
	parts := strings.SplitN(desc, ",", 3)
	if len(parts) >= 1 {
		employeeID = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 2 {
		engName = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		chineseName = strings.TrimSpace(parts[2])
	}
	return
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add upload-service/middleware.go upload-service/middleware_test.go
git commit -m "feat(upload-service): add request-id, access-log, otel, auth middleware"
```

---

## Task 10: upload handler

**Files:**
- Create: `upload-service/handler.go`
- Test: `upload-service/handler_test.go`

- [ ] **Step 1: Write the failing test**

Create `upload-service/handler_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hmchangw/chat/pkg/drive"
	"github.com/hmchangw/chat/pkg/model"
)

// fakeDrive implements driveClient for handler tests.
type fakeDrive struct {
	uploadResp []drive.UploadGroupImageResponse
	uploadErr  error
	uploadGot  struct{ userID, username, email, groupID, origin string; n int }

	getResp *drive.GetGroupImageResponse
	getErr  error
	getGot  struct{ host, groupID, fileID string }

	baseURL string
}

func (f *fakeDrive) UploadGroupImages(userID, username, email, groupID, origin string, files []drive.MultipartFile) ([]drive.UploadGroupImageResponse, error) {
	f.uploadGot.userID, f.uploadGot.username, f.uploadGot.email = userID, username, email
	f.uploadGot.groupID, f.uploadGot.origin, f.uploadGot.n = groupID, origin, len(files)
	return f.uploadResp, f.uploadErr
}
func (f *fakeDrive) GetGroupImage(host, groupID, fileID string) (*drive.GetGroupImageResponse, error) {
	f.getGot.host, f.getGot.groupID, f.getGot.fileID = host, groupID, fileID
	return f.getResp, f.getErr
}
func (f *fakeDrive) GetBaseURLFromRoomOrigin(string) string { return f.baseURL }

func multipartBody(t *testing.T, field string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for name, data := range files {
		w, err := mw.CreateFormFile(field, name)
		require.NoError(t, err)
		_, _ = w.Write(data)
	}
	require.NoError(t, mw.Close())
	return body, mw.FormDataContentType()
}

func newUploadCtx(t *testing.T, roomID string, body *bytes.Buffer, contentType string, user *AuthenticatedUser) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v4/rooms/"+roomID+"/upload/protected-images", body)
	req.Header.Set("Content-Type", contentType)
	c.Request = req
	if roomID != "" {
		c.Params = gin.Params{{Key: "roomId", Value: roomID}}
	}
	if user != nil {
		c.Set(ctxUserKey, user)
	}
	return c, w
}

func okUser() *AuthenticatedUser {
	return &AuthenticatedUser{User: model.User{Account: "alice", EngName: "Alice", ChineseName: "陳"}, Email: "alice@x.com"}
}

func TestUpload_MissingRoomID_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	c, w := newUploadCtx(t, "", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidPathParam, decodeErr(t, w).ErrorType)
}

func TestUpload_NoUserInContext_500(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	c, w := newUploadCtx(t, "r1", body, ct, nil)
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, errTypeInvalidUser, decodeErr(t, w).ErrorType)
}

func TestUpload_NoEmail_500(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	u := okUser()
	u.Email = ""
	c, w := newUploadCtx(t, "r1", body, ct, u)
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, errTypeInvalidUser, decodeErr(t, w).ErrorType)
}

func TestUpload_NotMember_403(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(false, nil)
	h := NewHandler(store, &fakeDrive{}, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, errTypeUserNotInRoom, decodeErr(t, w).ErrorType)
}

func TestUpload_RoomNotFound_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(nil, ErrRoomNotFound)
	h := NewHandler(store, &fakeDrive{}, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidRoomID, decodeErr(t, w).ErrorType)
}

func TestUpload_NotMultipart_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-x"}, nil)
	h := NewHandler(store, &fakeDrive{}, 10)
	c, w := newUploadCtx(t, "r1", bytes.NewBufferString("not-multipart"), "text/plain", okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidFormParam, decodeErr(t, w).ErrorType)
}

func TestUpload_TooManyFiles_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-x"}, nil)
	h := NewHandler(store, &fakeDrive{}, 1) // limit 1
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x"), "b.png": []byte("y")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidFormParam, decodeErr(t, w).ErrorType)
}

func TestUpload_AllRejected_EarlyExit_NoDriveCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-x"}, nil)
	fd := &fakeDrive{}
	h := NewHandler(store, fd, 10)
	// .exe is an invalid type -> rejected in preprocessing.
	body, ct := multipartBody(t, "images", map[string][]byte{"big.exe": []byte("x")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, fd.uploadGot.n, "drive must not be called when all files are rejected")
	var got struct {
		Results []drive.UploadProtectedImageResponse `json:"results"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Results, 1)
	assert.Equal(t, "failure", got.Results[0].Status)
	assert.Equal(t, "file has an invalid file type", got.Results[0].Error)
}

func TestUpload_MixedSuccessAndFailure_Merges(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-x"}, nil)
	fd := &fakeDrive{
		baseURL: "https://drive.example.com",
		uploadResp: []drive.UploadGroupImageResponse{
			{Status: "Success", File: drive.GroupImageObject{FileID: "img-xyz", GroupID: "r1", Filename: "a.png"}},
		},
	}
	h := NewHandler(store, fd, 10)
	// one valid (a.png), one invalid (big.exe).
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x"), "big.exe": []byte("y")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, fd.uploadGot.n, "only the valid file reaches drive")
	assert.Equal(t, "alice", fd.uploadGot.userID)
	assert.Equal(t, "Alice 陳", fd.uploadGot.username)
	assert.Equal(t, "alice@x.com", fd.uploadGot.email)
	assert.Equal(t, "site-x", fd.uploadGot.origin)

	var got struct {
		Results []drive.UploadProtectedImageResponse `json:"results"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Results, 2)
	var success, failure drive.UploadProtectedImageResponse
	for _, r := range got.Results {
		if r.Status == "Success" {
			success = r
		} else {
			failure = r
		}
	}
	assert.Equal(t, "a.png", success.Name)
	assert.Equal(t, "api/v4/rooms/r1/protected-image/img-xyz?drive_host=https://drive.example.com", success.RelativePath)
	assert.Equal(t, "big.exe", failure.Name)
	assert.Equal(t, "file has an invalid file type", failure.Error)
}

func TestUpload_DriveError_500(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	store.EXPECT().GetRoom(gomock.Any(), "r1").Return(&model.Room{ID: "r1", SiteID: "site-x"}, nil)
	fd := &fakeDrive{uploadErr: errors.New("boom")}
	h := NewHandler(store, fd, 10)
	body, ct := multipartBody(t, "images", map[string][]byte{"a.png": []byte("x")})
	c, w := newUploadCtx(t, "r1", body, ct, okUser())
	h.HandleUploadProtectedImages(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, errTypeInternal, decodeErr(t, w).ErrorType)
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var e errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &e))
	return e
}
```

> `strings` is intentionally NOT imported yet — Task 11 adds it when the download tests first use it.

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=upload-service`
Expected: FAIL — `NewHandler`/`driveClient`/`HandleUploadProtectedImages` undefined.

- [ ] **Step 3: Implement `handler.go`**

Create `upload-service/handler.go`:

```go
package main

import (
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hmchangw/chat/pkg/drive"
)

// driveClient is the subset of the Drive client the handlers use.
type driveClient interface {
	UploadGroupImages(userID, username, email, groupID, origin string, files []drive.MultipartFile) ([]drive.UploadGroupImageResponse, error)
	GetGroupImage(host, groupID, fileID string) (*drive.GetGroupImageResponse, error)
	GetBaseURLFromRoomOrigin(origin string) string
}

// Handler holds the upload-service dependencies.
type Handler struct {
	store    Store
	drive    driveClient
	maxFiles int
}

// NewHandler wires the handler dependencies.
func NewHandler(store Store, dc driveClient, maxFiles int) *Handler {
	return &Handler{store: store, drive: dc, maxFiles: maxFiles}
}

// HandleHealth is the liveness probe.
func (h *Handler) HandleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// HandleUploadProtectedImages uploads protected images for a room on behalf of
// the authenticated user, returning per-file success/failure in a single 200.
func (h *Handler) HandleUploadProtectedImages(c *gin.Context) {
	roomID := c.Param("roomId")
	if roomID == "" {
		abortError(c, http.StatusBadRequest, errTypeInvalidPathParam, "roomId: required")
		return
	}

	user, ok := userFromContext(c)
	if !ok {
		abortError(c, http.StatusInternalServerError, errTypeInvalidUser, "user not authenticated")
		return
	}
	if user.Email == "" {
		abortError(c, http.StatusInternalServerError, errTypeInvalidUser, "the user has no email provided")
		return
	}

	member, err := h.store.IsMember(c.Request.Context(), roomID, user.Account)
	if err != nil {
		slog.Error("membership check failed", "error", err, "room_id", roomID)
		abortError(c, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	if !member {
		abortError(c, http.StatusForbidden, errTypeUserNotInRoom,
			fmt.Sprintf("user %s is not in room %s", user.Account, roomID))
		return
	}

	room, err := h.store.GetRoom(c.Request.Context(), roomID)
	if err != nil {
		if errIsRoomNotFound(err) {
			abortError(c, http.StatusBadRequest, errTypeInvalidRoomID, "room id not exists")
			return
		}
		slog.Error("get room failed", "error", err, "room_id", roomID)
		abortError(c, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		abortError(c, http.StatusBadRequest, errTypeInvalidFormParam, "request must be multipart/form-data")
		return
	}
	files := form.File["images"]
	if len(files) > h.maxFiles {
		abortError(c, http.StatusBadRequest, errTypeInvalidFormParam, "too many files")
		return
	}

	results, fileHeaders, opened := preprocessFiles(files)
	for _, f := range opened {
		defer f.Close()
	}

	if len(fileHeaders) == 0 {
		c.JSON(http.StatusOK, gin.H{"results": results})
		return
	}

	responses, err := h.drive.UploadGroupImages(user.Account, user.DisplayName(), user.Email, roomID, room.SiteID, fileHeaders)
	if err != nil {
		slog.Error("drive upload failed", "error", err, "room_id", roomID)
		abortError(c, http.StatusInternalServerError, errTypeInternal, "failed to upload images")
		return
	}

	driveHost := h.drive.GetBaseURLFromRoomOrigin(room.SiteID)
	for _, resp := range responses {
		item := drive.UploadProtectedImageResponse{
			Name:   resp.File.Filename,
			Status: resp.Status,
			Error:  resp.Error,
		}
		if resp.Status == driveStatusSuccess {
			item.RelativePath = fmt.Sprintf("api/v4/rooms/%s/protected-image/%s?drive_host=%s",
				resp.File.GroupID, resp.File.FileID, driveHost)
		}
		results = append(results, item)
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}

// preprocessFiles runs the size/extension/open checks. Rejected files become
// failure results; accepted files become MultipartFiles plus their open handles
// (for deferred closing by the caller).
func preprocessFiles(files []*multipart.FileHeader) (results []drive.UploadProtectedImageResponse, fileHeaders []drive.MultipartFile, opened []multipart.File) {
	for _, fh := range files {
		if fh.Size > drive.UploadImageMaxSizeBytes {
			results = append(results, drive.UploadProtectedImageResponse{
				Name: fh.Filename, Status: statusFailure, Error: "file size exceeds limit"})
			continue
		}
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if !drive.AllowedImageFileTypes[ext] {
			results = append(results, drive.UploadProtectedImageResponse{
				Name: fh.Filename, Status: statusFailure, Error: "file has an invalid file type"})
			continue
		}
		f, err := fh.Open()
		if err != nil {
			results = append(results, drive.UploadProtectedImageResponse{
				Name: fh.Filename, Status: statusFailure, Error: "failed to open file"})
			continue
		}
		fileHeaders = append(fileHeaders, drive.MultipartFile{File: f, Filename: fh.Filename})
		opened = append(opened, f)
	}
	return results, fileHeaders, opened
}
```

Add a tiny helper at the bottom of `store.go` for the sentinel check (keeps `errors` import local to store files):

```go
// errIsRoomNotFound reports whether err wraps ErrRoomNotFound.
func errIsRoomNotFound(err error) bool { return errors.Is(err, ErrRoomNotFound) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=upload-service`
Expected: PASS (all upload subtests green).

- [ ] **Step 5: Commit**

```bash
git add upload-service/handler.go upload-service/store.go upload-service/handler_test.go
git commit -m "feat(upload-service): add protected-image upload handler"
```

---

## Task 11: download handler

**Files:**
- Modify: `upload-service/handler.go`
- Test: `upload-service/handler_test.go`

- [ ] **Step 1: Write the failing test**

First add `"strings"` to the `upload-service/handler_test.go` import block (alphabetically, after `net/http/httptest`). Then append:

```go
type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

func newDownloadCtx(t *testing.T, roomID, fileID, driveHost string, user *AuthenticatedUser) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	url := "/api/v4/rooms/" + roomID + "/protected-image/" + fileID
	if driveHost != "" {
		url += "?drive_host=" + driveHost
	}
	c.Request = httptest.NewRequest(http.MethodGet, url, nil)
	var params gin.Params
	if roomID != "" {
		params = append(params, gin.Param{Key: "roomId", Value: roomID})
	}
	if fileID != "" {
		params = append(params, gin.Param{Key: "fileId", Value: fileID})
	}
	c.Params = params
	if user != nil {
		c.Set(ctxUserKey, user)
	}
	return c, w
}

func TestDownload_MissingFileID_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	c, w := newDownloadCtx(t, "r1", "", "https://d.example.com", okUser())
	h.HandleDownloadProtectedImage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidPathParam, decodeErr(t, w).ErrorType)
}

func TestDownload_MissingDriveHost_400(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	c, w := newDownloadCtx(t, "r1", "f1", "", okUser())
	h.HandleDownloadProtectedImage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errTypeInvalidPathParam, decodeErr(t, w).ErrorType)
}

func TestDownload_NoUser_500(t *testing.T) {
	ctrl := gomock.NewController(t)
	h := NewHandler(NewMockStore(ctrl), &fakeDrive{}, 10)
	c, w := newDownloadCtx(t, "r1", "f1", "https://d.example.com", nil)
	h.HandleDownloadProtectedImage(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, errTypeInvalidUser, decodeErr(t, w).ErrorType)
}

func TestDownload_NotMember_403(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(false, nil)
	h := NewHandler(store, &fakeDrive{}, 10)
	c, w := newDownloadCtx(t, "r1", "f1", "https://d.example.com", okUser())
	h.HandleDownloadProtectedImage(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, errTypeUserNotInRoom, decodeErr(t, w).ErrorType)
}

func TestDownload_DriveError_502(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	fd := &fakeDrive{getErr: errors.New("image not found")}
	h := NewHandler(store, fd, 10)
	c, w := newDownloadCtx(t, "r1", "f1", "https://d.example.com", okUser())
	h.HandleDownloadProtectedImage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, errTypeInternal, decodeErr(t, w).ErrorType)
}

func TestDownload_Success_StreamsBinary(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockStore(ctrl)
	store.EXPECT().IsMember(gomock.Any(), "r1", "alice").Return(true, nil)
	fd := &fakeDrive{getResp: &drive.GetGroupImageResponse{
		Reader:        readCloser{strings.NewReader("PNGDATA")},
		ContentType:   "image/png",
		ContentLength: 7,
	}}
	h := NewHandler(store, fd, 10)
	c, w := newDownloadCtx(t, "r1", "f1", "https://d.example.com", okUser())
	h.HandleDownloadProtectedImage(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "PNGDATA", w.Body.String())
	assert.Equal(t, "https://d.example.com", fd.getGot.host)
	assert.Equal(t, "r1", fd.getGot.groupID)
	assert.Equal(t, "f1", fd.getGot.fileID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `make test SERVICE=upload-service`
Expected: FAIL — `HandleDownloadProtectedImage` undefined.

- [ ] **Step 3: Implement the download handler**

Append to `upload-service/handler.go` (no new imports needed — `GetGroupImage` already defaults `ContentType`, and `c.DataFromReader` handles the streaming):

```go
// HandleDownloadProtectedImage proxies a protected image: it resolves a signed
// URL from Drive, fetches the bytes, and streams them straight to the client.
func (h *Handler) HandleDownloadProtectedImage(c *gin.Context) {
	roomID := c.Param("roomId")
	if roomID == "" {
		abortError(c, http.StatusBadRequest, errTypeInvalidPathParam, "roomId: required")
		return
	}
	fileID := c.Param("fileId")
	if fileID == "" {
		abortError(c, http.StatusBadRequest, errTypeInvalidPathParam, "fileId: required")
		return
	}
	driveHost := c.Query("drive_host")
	if driveHost == "" {
		abortError(c, http.StatusBadRequest, errTypeInvalidPathParam, "drive_host: required")
		return
	}

	user, ok := userFromContext(c)
	if !ok {
		abortError(c, http.StatusInternalServerError, errTypeInvalidUser, "user not authenticated")
		return
	}

	member, err := h.store.IsMember(c.Request.Context(), roomID, user.Account)
	if err != nil {
		slog.Error("membership check failed", "error", err, "room_id", roomID)
		abortError(c, http.StatusInternalServerError, errTypeInternal, "internal error")
		return
	}
	if !member {
		abortError(c, http.StatusForbidden, errTypeUserNotInRoom,
			fmt.Sprintf("user %s is not in room %s", user.Account, roomID))
		return
	}

	img, err := h.drive.GetGroupImage(driveHost, roomID, fileID)
	if err != nil {
		slog.Error("drive get image failed", "error", err, "room_id", roomID, "file_id", fileID)
		abortError(c, http.StatusBadGateway, errTypeInternal, "image not found")
		return
	}
	defer img.Reader.Close()

	// GetGroupImage already defaults ContentType to application/octet-stream, so
	// stream the body straight through with no intermediate buffering.
	c.DataFromReader(http.StatusOK, img.ContentLength, img.ContentType, img.Reader, map[string]string{})
}
```

(The `strings` import added in Step 1 is now used by `readCloser`/`strings.NewReader`, so the file compiles cleanly.)

- [ ] **Step 4: Run test to verify it passes**

Run: `make test SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add upload-service/handler.go upload-service/handler_test.go
git commit -m "feat(upload-service): add protected-image download proxy handler"
```

---

## Task 12: routes + main wiring

**Files:**
- Create: `upload-service/routes.go`, `upload-service/main.go`

- [ ] **Step 1: Implement `routes.go`**

Create `upload-service/routes.go`:

```go
package main

import "github.com/gin-gonic/gin"

// registerRoutes wires health plus the authenticated, traced /api/v4 group.
func registerRoutes(r *gin.Engine, h *Handler, v TokenValidator, devMode bool) {
	r.GET("/healthz", h.HandleHealth)

	api := r.Group("/api/v4")
	api.Use(otelMiddleware())
	api.Use(authMiddleware(v, devMode))
	api.POST("/rooms/:roomId/upload/protected-images", h.HandleUploadProtectedImages)
	api.GET("/rooms/:roomId/protected-image/:fileId", h.HandleDownloadProtectedImage)
}
```

- [ ] **Step 2: Implement `main.go`**

Create `upload-service/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/gin-gonic/gin"

	"github.com/hmchangw/chat/pkg/drive"
	"github.com/hmchangw/chat/pkg/mongoutil"
	pkgoidc "github.com/hmchangw/chat/pkg/oidc"
	"github.com/hmchangw/chat/pkg/otelutil"
	"github.com/hmchangw/chat/pkg/shutdown"
)

type config struct {
	Port    string `env:"PORT"      envDefault:"8080"`
	DevMode bool   `env:"DEV_MODE"  envDefault:"false"`
	SiteID  string `env:"SITE_ID,required"`

	MongoURI      string `env:"MONGO_URI,required"`
	MongoDB       string `env:"MONGO_DB"        envDefault:"chat"`
	MongoUsername string `env:"MONGO_USERNAME"  envDefault:""`
	MongoPassword string `env:"MONGO_PASSWORD"  envDefault:""`

	MaxFiles int `env:"MAX_FILES" envDefault:"10"`

	OIDCIssuerURL string   `env:"OIDC_ISSUER_URL"`
	OIDCAudiences []string `env:"OIDC_AUDIENCES" envSeparator:","`
	TLSSkipVerify bool     `env:"TLS_SKIP_VERIFY" envDefault:"false"`

	Drive drive.Config `envPrefix:"DRIVE_"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx := context.Background()
	cfg.Drive.LoadBaseURLs()

	tracerShutdown, err := otelutil.InitTracer(ctx, "upload-service")
	if err != nil {
		return fmt.Errorf("init tracer: %w", err)
	}

	mongoClient, err := mongoutil.Connect(ctx, cfg.MongoURI, cfg.MongoUsername, cfg.MongoPassword)
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	store := NewMongoStore(mongoClient.Database(cfg.MongoDB))
	driveClient := drive.NewClient(&cfg.Drive)

	var validator TokenValidator
	if !cfg.DevMode {
		if cfg.OIDCIssuerURL == "" || len(cfg.OIDCAudiences) == 0 {
			return fmt.Errorf("OIDC_ISSUER_URL and OIDC_AUDIENCES are required when DEV_MODE is false")
		}
		v, err := pkgoidc.NewValidator(ctx, pkgoidc.Config{
			IssuerURL:     cfg.OIDCIssuerURL,
			Audiences:     cfg.OIDCAudiences,
			TLSSkipVerify: cfg.TLSSkipVerify,
		})
		if err != nil {
			return fmt.Errorf("create oidc validator: %w", err)
		}
		validator = v
	}

	handler := NewHandler(store, driveClient, cfg.MaxFiles)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware())
	r.Use(accessLogMiddleware())
	registerRoutes(r, handler, validator, cfg.DevMode)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // downloads stream potentially-large bodies
	}

	srvErr := make(chan error, 1)
	go func() {
		slog.Info("upload service starting", "addr", addr, "site", cfg.SiteID)
		srvErr <- srv.ListenAndServe()
	}()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		shutdown.Wait(ctx, 25*time.Second,
			func(ctx context.Context) error { return srv.Shutdown(ctx) },
			func(ctx context.Context) error { return tracerShutdown(ctx) },
			func(ctx context.Context) error { mongoutil.Disconnect(ctx, mongoClient); return nil },
		)
	}()

	err = <-srvErr
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen upload server: %w", err)
	}
	<-shutdownDone
	return nil
}
```

- [ ] **Step 3: Verify build + vet + full service tests**

Run: `make build SERVICE=upload-service`
Expected: builds with no errors.

Run: `make test SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add upload-service/routes.go upload-service/main.go
git commit -m "feat(upload-service): add routes and main wiring"
```

---

## Task 13: deploy assets

**Files:**
- Create: `upload-service/deploy/Dockerfile`, `upload-service/deploy/docker-compose.yml`, `upload-service/deploy/azure-pipelines.yml`

- [ ] **Step 1: Dockerfile**

Create `upload-service/deploy/Dockerfile`:

```dockerfile
FROM golang:1.25.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/ pkg/
COPY upload-service/ upload-service/
RUN CGO_ENABLED=0 go build -o /upload-service ./upload-service/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=builder /upload-service /upload-service
USER app
ENTRYPOINT ["/upload-service"]
```

- [ ] **Step 2: docker-compose.yml**

Create `upload-service/deploy/docker-compose.yml`:

```yaml
name: upload-service

services:
  upload-service:
    build:
      context: ../..
      dockerfile: upload-service/deploy/Dockerfile
    ports:
      - "8085:8080"
    env_file:
      - path: ../../docker-local/.env
        required: false
    environment:
      - PORT=8080
      - DEV_MODE=${DEV_MODE:-true}
      - SITE_ID=${SITE_ID:-site-local}
      - MONGO_URI=mongodb://mongo:27017
      - MONGO_DB=chat
      - MAX_FILES=10
      - OIDC_ISSUER_URL=http://keycloak:8080/realms/chatapp
      - OIDC_AUDIENCES=nats-chat
      - TLS_SKIP_VERIFY=false
      - DRIVE_URL=${DRIVE_URL:-http://drive-mock:9000}
      - DRIVE_API_TOKEN=${DRIVE_API_TOKEN:-dev-token}
      - DRIVE_BASE_URL_CONFIG_PATH=/etc/config/baseurls.json
    networks:
      - chat-local

networks:
  chat-local:
    external: true
```

- [ ] **Step 3: azure-pipelines.yml**

Create `upload-service/deploy/azure-pipelines.yml`:

```yaml
trigger:
  branches:
    include:
      - main
      - develop
  paths:
    include:
      - upload-service/
      - pkg/

pr:
  branches:
    include:
      - main
  paths:
    include:
      - upload-service/
      - pkg/

variables:
  GO_VERSION: '1.25.10'
  SERVICE_NAME: upload-service
  REGISTRY: '$(containerRegistry)'

stages:
  - stage: Validate
    displayName: 'Lint & Test'
    jobs:
      - job: LintAndTest
        pool:
          vmImage: 'ubuntu-latest'
        steps:
          - task: GoTool@0
            inputs:
              version: '$(GO_VERSION)'
            displayName: 'Install Go $(GO_VERSION)'

          - script: go vet ./$(SERVICE_NAME)/... ./pkg/...
            displayName: 'Go Vet'

          - script: go test ./pkg/... -v -race -coverprofile=coverage-pkg.out
            displayName: 'Test shared packages'

          - script: go test ./$(SERVICE_NAME)/... -v -race -coverprofile=coverage-$(SERVICE_NAME).out
            displayName: 'Test $(SERVICE_NAME)'

          - script: go build -o /dev/null ./$(SERVICE_NAME)/
            displayName: 'Build $(SERVICE_NAME)'

  - stage: Build
    displayName: 'Build & Push Image'
    dependsOn: Validate
    condition: and(succeeded(), eq(variables['Build.SourceBranch'], 'refs/heads/main'))
    jobs:
      - job: BuildImage
        pool:
          vmImage: 'ubuntu-latest'
        steps:
          - task: Docker@2
            inputs:
              containerRegistry: '$(containerRegistry)'
              repository: 'chat/$(SERVICE_NAME)'
              command: 'buildAndPush'
              Dockerfile: '$(SERVICE_NAME)/deploy/Dockerfile'
              buildContext: '.'
              tags: |
                $(Build.BuildId)
                latest
            displayName: 'Build & push $(SERVICE_NAME)'
```

- [ ] **Step 4: Commit**

```bash
git add upload-service/deploy/
git commit -m "chore(upload-service): add Dockerfile, compose, azure pipeline"
```

---

## Task 14: client-api docs

**Files:**
- Modify: `docs/client-api.md`

- [ ] **Step 1: Locate the insertion point**

Run: `grep -n "^## " docs/client-api.md | head -40`
Pick the section that lists HTTP endpoints (near the `POST /auth` section, around the top). Add a new subsection after the auth endpoint documentation.

- [ ] **Step 2: Add the documentation**

Insert a new section documenting both endpoints. Use this exact content:

```markdown
## Protected Image Upload/Download (upload-service)

Both endpoints require the `ssoToken` header (validated via OIDC) and that the
caller is a member (has a subscription) of `:roomId`. Errors use the envelope
`{ "errorType": "<type>", "error": "<message>" }`.

### POST /api/v4/rooms/:roomId/upload/protected-images

Uploads protected inline images for a room. `Content-Type: multipart/form-data`;
repeat the form field `images` once per file.

| Param | Source | Description |
|-------|--------|-------------|
| `roomId` | path | Target room ID. |
| `images` | form (multi) | One or more image files (`.png/.jpeg/.jpg/.heic`, ≤ 25 MiB each, ≤ `MAX_FILES`). |

Success (200): every file reports success or failure in one body.

```json
{
  "results": [
    { "name": "pic1.png", "status": "Success", "relativePath": "api/v4/rooms/abc123/protected-image/img-xyz?drive_host=https://drive.example.com" },
    { "name": "big.exe", "status": "failure", "error": "file has an invalid file type" }
  ]
}
```

Per-file failure `error` values: `file size exceeds limit`, `file has an invalid file type`, `failed to open file`.

| Status | errorType | Condition |
|--------|-----------|-----------|
| 400 | `error-invalid-path-param` | Missing `roomId`. |
| 400 | `error-invalid-room-id` | Room does not exist. |
| 400 | `error-invalid-form-param` | Not multipart, or too many files. |
| 401 | `error-invalid-user` | Missing/invalid/expired `ssoToken`. |
| 403 | `error-user-not-in-room` | Caller is not a room member. |
| 500 | `error-invalid-user` | User missing in context, or no email on the account. |
| 500 | `error-internal` | Drive upload or store error. |

### GET /api/v4/rooms/:roomId/protected-image/:fileId

Downloads a protected image. The service proxies the bytes from Drive (signed
URL → fetch → stream). Typically called with the `relativePath` from the upload
response.

| Param | Source | Description |
|-------|--------|-------------|
| `roomId` | path | Room the image belongs to. |
| `fileId` | path | Drive file ID from the upload response. |
| `drive_host` | query | Drive base URL from the upload response. |

Success (200): raw image binary streamed (not JSON), with the upstream
`Content-Type` (defaulting to `application/octet-stream`).

| Status | errorType | Condition |
|--------|-----------|-----------|
| 400 | `error-invalid-path-param` | Missing `roomId`, `fileId`, or `drive_host`. |
| 401 | `error-invalid-user` | Missing/invalid/expired `ssoToken`. |
| 403 | `error-user-not-in-room` | Caller is not a room member. |
| 500 | `error-invalid-user` | User missing in context. |
| 502 | `error-internal` | Drive signer/download failure (image not found). |
```

- [ ] **Step 3: Commit**

```bash
git add docs/client-api.md
git commit -m "docs(client-api): document protected image upload/download endpoints"
```

---

## Task 15: full verification

**Files:** none (verification only).

- [ ] **Step 1: Format**

Run: `make fmt`
Expected: no diff, or only gofmt/goimports-normalized files (re-add + amend the last commit if anything changed).

- [ ] **Step 2: Lint**

Run: `make lint`
Expected: clean. If gosec runs inside lint and flags `G402`/`G304` in `pkg/drive`, confirm the `// #nosec` directives sit on the line directly above the flagged statement.

- [ ] **Step 3: Unit tests with race detector**

Run: `make test SERVICE=upload-service` then `make test SERVICE=pkg/drive` then `make test SERVICE=pkg/model`
Expected: all PASS.

- [ ] **Step 4: Coverage check (≥ 80%)**

Run:
```bash
go test ./upload-service/... -coverprofile=cov.out && go tool cover -func=cov.out | tail -1
go test ./pkg/drive/... -coverprofile=covd.out && go tool cover -func=covd.out | tail -1
```
Expected: total ≥ 80% for both. If `main.go` drags `upload-service` below 80% (it has no unit test), that is acceptable per the pattern in other services (main wiring is covered by build + integration), but the handler/middleware/store logic itself must be well above 80%. Add table cases if any handler branch is uncovered.

- [ ] **Step 5: SAST**

Run: `make sast`
Expected: no medium+ findings. The two `// #nosec` directives cover the intentional TLS-skip and config-file-read.

- [ ] **Step 6: Integration tests (Docker required)**

Run: `make test-integration SERVICE=upload-service`
Expected: PASS.

- [ ] **Step 7: Push**

```bash
git push -u origin claude/gracious-tesla-ijujl
```

---

## Self-Review

**Spec coverage:**
- `pkg/drive` config + LoadBaseURLs → Task 2. ✓
- `pkg/drive` types + ImagesFormData (renamed, `object` tag) → Task 3. ✓
- `pkg/drive` Client, routing, UploadGroupImages, presign + GetGroupImage → Tasks 4–6. ✓
- `model.User.DisplayName` (`*User`, nil-guard) → Task 1. ✓
- `ssoToken` header auth via `pkg/oidc`, identity from claims → Task 9. ✓
- In-repo OTel middleware → Task 9. ✓
- Store (IsMember/GetRoom) + Mongo + mocks → Task 8. ✓
- Upload handler full pipeline incl. early-exit + merge + v4 relativePath → Task 10. ✓
- Download proxy handler + 502 mapping + streaming → Task 11. ✓
- Structured error envelopes + per-endpoint status tables → Tasks 7, 10, 11, 14. ✓
- Routes/main/graceful shutdown → Task 12. ✓
- deploy assets → Task 13. ✓
- docs/client-api.md → Task 14. ✓
- TDD, coverage, lint, SAST → all tasks + Task 15. ✓

**Type consistency:** `Store.IsMember/GetRoom`, `driveClient.{UploadGroupImages,GetGroupImage,GetBaseURLFromRoomOrigin}`, `drive.UploadGroupImageResponse{Status,File,Error}` with `File GroupImageObject` (`json:"object"`), `drive.GetGroupImageResponse{Reader,ContentType,ContentLength}`, `AuthenticatedUser{model.User, Email}`, `errorResponse{ErrorType,Error}`, constants `errType*`, `statusFailure`, `driveStatusSuccess` — all used consistently across tasks.

**Placeholder scan:** No TBD/TODO and no `var _` import-scaffolding hacks. Cross-task import additions are called out explicitly (uploader_test.go import block in Task 5; handler_test.go `strings` import in Task 11).
