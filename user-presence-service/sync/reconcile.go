package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hmchangw/chat/pkg/model"
	"github.com/hmchangw/chat/pkg/msgraph"
)

// callActivities are the Teams activities that map to our in-call status
// (call/meeting activities only, per design).
var callActivities = map[string]struct{}{
	"InACall":           {},
	"InAConferenceCall": {},
	"Presenting":        {},
}

// isInCall reports whether a Teams presence reflects an active call.
func isInCall(p msgraph.Presence) bool {
	_, ok := callActivities[p.Activity]
	return ok
}

// accountFromEmail returns the local part of an email when its domain matches
// (case-insensitive on domain); ok=false otherwise.
func accountFromEmail(email, domain string) (string, bool) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return "", false
	}
	if !strings.EqualFold(email[at+1:], domain) {
		return "", false
	}
	return email[:at], true
}

type reconcileConfig struct {
	SiteID      string
	EmailDomain string
	ExternalTTL time.Duration
}

type reconciler struct {
	active activeLister
	users  userResolver
	pres   presenceReader
	app    externalApplier
	idx    inCallIndex
	idm    idMapStore
	pub    statePublisher
	cfg    reconcileConfig
}

func newReconciler(active activeLister, users userResolver, pres presenceReader, app externalApplier, idx inCallIndex, idm idMapStore, pub statePublisher, cfg reconcileConfig) *reconciler {
	return &reconciler{active: active, users: users, pres: pres, app: app, idx: idx, idm: idm, pub: pub, cfg: cfg}
}

// run performs one full reconciliation: take the currently-active accounts
// (those with a live connection — only they can be shown in-call), resolve them
// to Azure IDs, read Teams presence, then set in-call accounts and clear those
// no longer in a call. A single account's failure is logged and skipped rather
// than failing the whole job.
func (r *reconciler) run(ctx context.Context) error {
	accounts, err := r.active.ActiveAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list active accounts: %w", err)
	}

	idByAccount, err := r.resolveIDs(ctx, accounts)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(idByAccount))
	accountByID := make(map[string]string, len(idByAccount))
	for account, id := range idByAccount {
		ids = append(ids, id)
		accountByID[id] = account
	}

	presences, err := r.pres.GetPresencesByUserId(ctx, ids)
	if err != nil {
		return fmt.Errorf("get presences: %w", err)
	}
	current := make(map[string]struct{}, len(presences))
	for _, p := range presences {
		if !isInCall(p) {
			continue
		}
		if account, ok := accountByID[p.ID]; ok {
			current[account] = struct{}{}
		}
	}

	prev, err := r.idx.Members(ctx)
	if err != nil {
		return fmt.Errorf("read in-call index: %w", err)
	}

	var failures int
	for account := range current {
		if err := r.apply(ctx, account, model.StatusInCall); err != nil {
			slog.Error("apply in-call failed", "account", account, "error", err)
			failures++
		}
	}
	for _, account := range prev {
		if _, still := current[account]; still {
			continue
		}
		if err := r.apply(ctx, account, model.StatusNone); err != nil {
			slog.Error("clear in-call failed", "account", account, "error", err)
			failures++
		}
	}

	slog.Info("teams presence reconcile complete",
		"site", r.cfg.SiteID, "active", len(accounts), "inCall", len(current), "failures", failures)
	return nil
}

// resolveIDs maps accounts to Azure object ids via the permanent id-map cache,
// lazily filling any accounts missing from it. The mapping is immutable, so
// there is no periodic refresh — Graph is queried only when a new active user
// is not yet cached.
func (r *reconciler) resolveIDs(ctx context.Context, accounts []string) (map[string]string, error) {
	idByAccount, err := r.idm.Resolve(ctx, accounts)
	if err != nil {
		return nil, fmt.Errorf("resolve id map: %w", err)
	}
	var missing []string
	for _, a := range accounts {
		if _, ok := idByAccount[a]; !ok {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return idByAccount, nil
	}
	filled := r.fetchIDs(ctx, missing)
	if len(filled) == 0 {
		return idByAccount, nil
	}
	// Persisting is best-effort: the in-memory map still serves this run, and a
	// failed write is simply retried next run.
	if err := r.idm.Store(ctx, filled); err != nil {
		slog.Error("persist id map failed", "error", err)
	}
	for a, id := range filled {
		idByAccount[a] = id
	}
	return idByAccount, nil
}

// fetchIDs resolves only the cache-missing accounts to Azure object ids via a
// targeted per-account Graph lookup (account@domain) — no tenant-wide
// enumeration. A single account's lookup failure is logged and skipped so it
// never fails the whole job; an account with no Graph user is skipped until it
// next appears.
func (r *reconciler) fetchIDs(ctx context.Context, missing []string) map[string]string {
	out := make(map[string]string, len(missing))
	for _, account := range missing {
		upn := account + "@" + r.cfg.EmailDomain
		u, err := r.users.GetUserByPrincipalName(ctx, upn)
		if err != nil {
			slog.Error("resolve azure id failed", "account", account, "error", err)
			continue
		}
		if u == nil || u.ID == "" {
			continue
		}
		out[account] = u.ID
	}
	return out
}

// apply sets/clears the external status, updates the in-call index, and
// publishes a state change only when the effective status changed.
func (r *reconciler) apply(ctx context.Context, account string, status model.PresenceStatus) error {
	changed, eff, err := r.app.SetExternal(ctx, account, status, r.cfg.ExternalTTL)
	if err != nil {
		return fmt.Errorf("set external %q: %w", account, err)
	}
	if status == model.StatusNone {
		if err := r.idx.Remove(ctx, account); err != nil {
			return fmt.Errorf("index remove %q: %w", account, err)
		}
	} else {
		if err := r.idx.Add(ctx, account); err != nil {
			return fmt.Errorf("index add %q: %w", account, err)
		}
	}
	if changed {
		r.pub.Publish(ctx, account, eff)
	}
	return nil
}
