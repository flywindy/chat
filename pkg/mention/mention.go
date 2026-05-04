package mention

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hmchangw/chat/pkg/model"
)

// mentionRe matches @mention tokens in message content. The @ must appear at
// the start of the content or immediately after whitespace, so email-style
// occurrences (e.g. "bob@example.com", "here@all") are not treated as mentions.
var mentionRe = regexp.MustCompile(`(^|\s)@([0-9a-zA-Z_-]+(\.[0-9a-zA-Z_-]+)*)`)

// ParseResult holds parsed mention data extracted from message content.
type ParseResult struct {
	Accounts   []string // unique mentioned accounts, lowercased, excluding @all
	MentionAll bool     // true if @all was mentioned (case-insensitive)
}

// Parse extracts @mention tokens from content and returns the unique
// mentioned accounts along with whether @all was present.
func Parse(content string) ParseResult {
	matches := mentionRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return ParseResult{}
	}

	var result ParseResult
	seen := make(map[string]struct{}, len(matches))

	for _, m := range matches {
		account := strings.ToLower(m[2])
		if account == "all" {
			result.MentionAll = true
			continue
		}
		if _, exists := seen[account]; !exists {
			seen[account] = struct{}{}
			result.Accounts = append(result.Accounts, account)
		}
	}

	return result
}

// LookupFunc abstracts user-by-account lookup so Resolve is testable without a real DB.
type LookupFunc func(ctx context.Context, accounts []string) ([]model.User, error)

// ResolveResult holds mention resolution output.
type ResolveResult struct {
	Participants []model.Participant // enriched mentioned users + @all entry if present
	MentionAll   bool                // true if @all or @here was mentioned
	Accounts     []string            // raw parsed accounts (for caller use outside resolution)
}

// Resolve parses @mentions from content, looks up users via lookupFn,
// and builds Participants. On lookup error, returns partial result
// (MentionAll and Accounts populated, Participants empty) with the error.
func Resolve(ctx context.Context, content string, lookupFn LookupFunc) (*ResolveResult, error) {
	parsed := Parse(content)
	result := &ResolveResult{
		MentionAll: parsed.MentionAll,
		Accounts:   parsed.Accounts,
	}
	if len(parsed.Accounts) == 0 && !parsed.MentionAll {
		return result, nil
	}

	if len(parsed.Accounts) > 0 {
		users, err := lookupFn(ctx, parsed.Accounts)
		if err != nil {
			return result, fmt.Errorf("find mentioned users: %w", err)
		}
		for i := range users {
			result.Participants = append(result.Participants, model.Participant{
				UserID:      users[i].ID,
				Account:     users[i].Account,
				SiteID:      users[i].SiteID,
				ChineseName: users[i].ChineseName,
				EngName:     users[i].EngName,
			})
		}
	}

	if parsed.MentionAll {
		result.Participants = append(result.Participants, model.Participant{
			Account: "all",
			EngName: "all",
		})
	}

	return result, nil
}
