package searchengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type bulkActionMeta struct {
	Index       string `json:"_index"`
	ID          string `json:"_id"`
	Version     int64  `json:"version,omitempty"`
	VersionType string `json:"version_type,omitempty"`
}

type bulkResponse struct {
	Items []map[string]bulkItemResult `json:"items"`
}

type bulkItemResult struct {
	Status int `json:"status"`
	Error  struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
}

type httpAdapter struct {
	transport Transporter
}

func newAdapter(transport Transporter) *httpAdapter {
	return &httpAdapter{transport: transport}
}

func (a *httpAdapter) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return fmt.Errorf("create ping request: %w", err)
	}
	resp, err := a.transport.Perform(req)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (a *httpAdapter) Bulk(ctx context.Context, actions []BulkAction) ([]BulkResult, error) {
	var buf bytes.Buffer
	for _, action := range actions {
		meta := bulkActionMeta{
			Index:       action.Index,
			ID:          action.DocID,
			Version:     action.Version,
			VersionType: "external",
		}
		switch action.Action {
		case ActionIndex:
			line, _ := json.Marshal(map[string]bulkActionMeta{"index": meta})
			buf.Write(line)
			buf.WriteByte('\n')
			buf.Write(action.Doc)
			buf.WriteByte('\n')
		case ActionDelete:
			line, _ := json.Marshal(map[string]bulkActionMeta{"delete": meta})
			buf.Write(line)
			buf.WriteByte('\n')
		case ActionUpdate:
			// ES 8.x / OpenSearch 2.x bulk _update DOES accept version +
			// version_type=external. We intentionally omit them because
			// user-room scripted updates already enforce ordering via the
			// painless LWW guard (`params.ts > stored` against the stored
			// roomTimestamps), so layering external versioning on top would
			// be redundant and complicate 409-handling semantics. Omit Version
			// even when the caller sets it.
			updateMeta := bulkActionMeta{Index: action.Index, ID: action.DocID}
			line, _ := json.Marshal(map[string]bulkActionMeta{"update": updateMeta})
			buf.Write(line)
			buf.WriteByte('\n')
			buf.Write(action.Doc)
			buf.WriteByte('\n')
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/_bulk", &buf)
	if err != nil {
		return nil, fmt.Errorf("create bulk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := a.transport.Perform(req)
	if err != nil {
		return nil, fmt.Errorf("bulk request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("bulk: status %d, read body: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("bulk: status %d, body: %s", resp.StatusCode, body)
	}

	var bulkResp bulkResponse
	if err := json.NewDecoder(resp.Body).Decode(&bulkResp); err != nil {
		return nil, fmt.Errorf("decode bulk response: %w", err)
	}

	results := make([]BulkResult, len(bulkResp.Items))
	for i, item := range bulkResp.Items {
		for _, detail := range item {
			results[i] = BulkResult{
				Status:    detail.Status,
				ErrorType: detail.Error.Type,
				Error:     detail.Error.Reason,
			}
		}
	}
	return results, nil
}

func (a *httpAdapter) UpsertTemplate(ctx context.Context, name string, body json.RawMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("/_index_template/%s", name), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create template request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.transport.Perform(req)
	if err != nil {
		return fmt.Errorf("upsert template: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("upsert template: status %d, read body: %w", resp.StatusCode, readErr)
		}
		return fmt.Errorf("upsert template: status %d, body: %s", resp.StatusCode, respBody)
	}
	return nil
}

func (a *httpAdapter) GetIndexMapping(ctx context.Context, index string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/%s/_mapping", index), nil)
	if err != nil {
		return nil, fmt.Errorf("create mapping request: %w", err)
	}

	resp, err := a.transport.Perform(req)
	if err != nil {
		return nil, fmt.Errorf("get index mapping: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("get index mapping: status %d, read body: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("get index mapping: status %d, body: %s", resp.StatusCode, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read mapping response: %w", err)
	}
	return body, nil
}

// Search executes a `_search` against the comma-joined indices with
// `ignore_unavailable=true&allow_no_indices=true` so unknown remote clusters
// and missing local indices return empty hits rather than a 404/503.
func (a *httpAdapter) Search(ctx context.Context, indices []string, body json.RawMessage) (json.RawMessage, error) {
	if len(indices) == 0 {
		return nil, fmt.Errorf("search: indices required")
	}
	path := fmt.Sprintf("/%s/_search?ignore_unavailable=true&allow_no_indices=true", strings.Join(indices, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.transport.Perform(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("search: status %d, read body: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("search: status %d, body: %s", resp.StatusCode, respBody)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}
	return data, nil
}

// GetDoc fetches a single document by id. Returns (nil, false, nil) on 404.
//
// Both `index` and `docID` are URL-path-escaped defensively — ES doc IDs
// can legally contain `/`, `?`, `#`, or whitespace, and an un-escaped
// value there would malform the request path. The caller is expected to
// pass a single index name (not a comma-joined pattern) for this endpoint.
func (a *httpAdapter) GetDoc(ctx context.Context, index, docID string) (json.RawMessage, bool, error) {
	path := fmt.Sprintf("/%s/_doc/%s", url.PathEscape(index), url.PathEscape(docID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create get-doc request: %w", err)
	}

	resp, err := a.transport.Perform(req)
	if err != nil {
		return nil, false, fmt.Errorf("get-doc request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, false, fmt.Errorf("get-doc: status %d, read body: %w", resp.StatusCode, readErr)
		}
		return nil, false, fmt.Errorf("get-doc: status %d, body: %s", resp.StatusCode, respBody)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read get-doc response: %w", err)
	}
	return data, true, nil
}
