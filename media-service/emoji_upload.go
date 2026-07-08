package main

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	_ "image/gif" // register the GIF decoder (jpeg/png registered in upload.go)

	"github.com/hmchangw/chat/pkg/emoji"
	"github.com/hmchangw/chat/pkg/errcode"
	"github.com/hmchangw/chat/pkg/errcode/errhttp"
	"github.com/hmchangw/chat/pkg/model"
)

// emojiObjectKey is the MinIO key for an emoji blob; the bucket is shared with
// avatars, so the prefix namespaces it. siteID is included so the key carries
// the emoji's full identity — shortcodes are only unique per site.
func emojiObjectKey(siteID, shortcode string) string {
	return "emoji/" + siteID + "/" + shortcode
}

// emojiDocID is the deterministic custom_emojis document _id.
func emojiDocID(siteID, shortcode string) string { return siteID + ":" + shortcode }

// emojiImagePath is the canonical imageUrl stored on the doc and returned by
// list: "/api/v1/emoji/{shortcode}?siteid={siteID}" — self-describing so it
// serves correctly regardless of which cluster it's fetched from. The
// cross-site redirect target is built separately (no query param: the target
// always defaults to local). shortcode is charset-validated, so no escaping
// is needed.
func emojiImagePath(siteID, shortcode string) string {
	return "/api/v1/emoji/" + shortcode + "?siteid=" + siteID
}

// emojiUploadResponse is the 200 body on a successful upload.
type emojiUploadResponse struct {
	Shortcode   string `json:"shortcode"`
	ETag        string `json:"etag"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	UpdatedAt   int64  `json:"updatedAt"`
}

func (h *handler) HandleEmojiUpload(c *gin.Context) {
	ctx := c.Request.Context()
	c.Set("avatar_kind", "emoji")
	siteID := h.cfg.SiteID

	shortcode, err := emoji.Canonicalize(c.Param("shortcode"))
	if err != nil {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest("invalid emoji shortcode"))
		return
	}
	if emoji.IsStandard(shortcode) {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest(
			"shortcode collides with a built-in standard emoji",
			errcode.WithReason(errcode.EmojiShortcodeReserved)))
		return
	}

	// Size cap before reading the body.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.EmojiMaxUploadBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest("upload too large or unreadable"))
		return
	}

	// Header-only prefilter: reject oversized declared dimensions before the
	// full decode allocates pixel buffers (decompression-bomb hardening).
	cfgImg, cfgFormat, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (cfgFormat != "png" && cfgFormat != "jpeg" && cfgFormat != "gif") {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest("body is not a valid PNG, JPEG, or GIF image"))
		return
	}
	if cfgImg.Width > h.cfg.EmojiMaxDimension || cfgImg.Height > h.cfg.EmojiMaxDimension {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest(
			fmt.Sprintf("image exceeds %dx%d", h.cfg.EmojiMaxDimension, h.cfg.EmojiMaxDimension)))
		return
	}

	// Decode to confirm a real PNG/JPEG/GIF; animated GIFs decode as their
	// first frame, which is what the dimension check applies to.
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest("body is not a valid PNG, JPEG, or GIF image"))
		return
	}
	if b := img.Bounds(); b.Dx() > h.cfg.EmojiMaxDimension || b.Dy() > h.cfg.EmojiMaxDimension {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, errcode.BadRequest(
			fmt.Sprintf("image exceeds %dx%d", h.cfg.EmojiMaxDimension, h.cfg.EmojiMaxDimension)))
		return
	}
	contentType := "image/" + format

	// Store the object FIRST, then upsert the doc (doc exists ⟺ object exists).
	key := emojiObjectKey(siteID, shortcode)
	// A delete racing this upload can remove the doc AND this just-written
	// blob before the upsert below, leaving a doc without a blob until the
	// next upload; the serve path degrades to 404 (see emoji_serve.go).
	etag, err := h.blobs.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), contentType)
	if err != nil {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, fmt.Errorf("store emoji object: %w", err))
		return
	}
	now := time.Now().UTC().UnixMilli()
	uploader := c.Query("uploader") // v1: unauthenticated, audit-only (§7 client-api)
	if len(uploader) > 64 {
		uploader = uploader[:64]
	}
	e := &model.CustomEmoji{
		ID:          emojiDocID(siteID, shortcode),
		SiteID:      siteID,
		Shortcode:   shortcode,
		ImageURL:    emojiImagePath(siteID, shortcode),
		CreatedBy:   uploader,
		CreatedAt:   now,
		UpdatedBy:   uploader,
		UpdatedAt:   now,
		MinioKey:    key,
		ContentType: contentType,
		Size:        int64(len(raw)),
		ETag:        etag,
	}
	if err := h.emojis.UpsertEmoji(ctx, e); err != nil {
		c.Set("avatar_outcome", "error")
		errhttp.Write(ctx, c, fmt.Errorf("upsert emoji doc: %w", err))
		return
	}
	c.Set("avatar_outcome", "upload")
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, emojiUploadResponse{
		Shortcode:   shortcode,
		ETag:        e.ETag,
		ContentType: e.ContentType,
		Size:        e.Size,
		UpdatedAt:   e.UpdatedAt,
	})
}
