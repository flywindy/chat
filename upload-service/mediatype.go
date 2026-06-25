package main

import "strings"

// mediaTypeFilter decides whether an uploaded MIME type is allowed: blacklist
// first (deny wins), then whitelist (when non-empty, the type must match).
type mediaTypeFilter struct {
	whitelist []string
	blacklist []string
}

func newMediaTypeFilter(whitelist, blacklist string) *mediaTypeFilter {
	return &mediaTypeFilter{whitelist: parseMediaTypes(whitelist), blacklist: parseMediaTypes(blacklist)}
}

func parseMediaTypes(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = normalizeMediaType(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeMediaType lowercases, trims, and drops any parameters after the first
// ";" (e.g. "Image/SVG+XML; charset=utf-8" → "image/svg+xml") so a parameterized
// Content-Type can't slip past an exact-match allow/deny rule.
func normalizeMediaType(v string) string {
	if base, _, ok := strings.Cut(v, ";"); ok {
		v = base
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func (f *mediaTypeFilter) allowed(mime string) bool {
	m := normalizeMediaType(mime)
	for _, b := range f.blacklist {
		if matchMediaType(b, m) {
			return false
		}
	}
	if len(f.whitelist) == 0 {
		return true
	}
	for _, w := range f.whitelist {
		if matchMediaType(w, m) {
			return true
		}
	}
	return false
}

// matchMediaType supports exact match, "type/*" prefix wildcard, and bare "*".
func matchMediaType(pattern, mime string) bool {
	if pattern == "*" || pattern == "*/*" {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(mime, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == mime
}
