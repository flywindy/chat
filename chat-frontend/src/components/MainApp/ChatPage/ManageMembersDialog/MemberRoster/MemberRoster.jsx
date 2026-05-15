import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNats } from '@/context/NatsContext'
import {
  listRoomMembers,
  listOrgMembers,
  removeMember,
  updateMemberRole,
} from '@/api'
import { ROLE_OWNER, ROLE_MEMBER } from '@/lib/constants'
import { formatAsyncJobError } from '@/api'
import './style.css'

export default function MemberRoster({ room }) {
  const nats = useNats()
  const { user } = nats
  const account = user?.account
  const [members, setMembers] = useState([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [busyKey, setBusyKey] = useState(null)
  const [actionError, setActionError] = useState(null)
  // Org-expansion state. Each map is keyed by orgId.
  //   expandedOrgs   — UI state: which orgs are currently open
  //   orgChildren    — cached responses from orgs.{id}.members; survives a
  //                    collapse/re-expand cycle so we don't refetch.
  //   orgFetchState  — 'loading' | 'error' while a fetch is in flight or
  //                    after it failed (so the user can see a banner). On
  //                    success the key is removed.
  const [expandedOrgs, setExpandedOrgs] = useState({})
  const [orgChildren, setOrgChildren] = useState({})
  const [orgFetchState, setOrgFetchState] = useState({})

  // Generation refs guard against stale async writes when the request
  // identity changes mid-fetch. memberListGenRef bumps every time
  // fetchMembers is invoked (room change or post-action refetch). Each
  // org has its own generation key in orgFetchGenRef.current[orgId] so a
  // user can rapidly expand/collapse the same org without the previous
  // fetch's resolution stomping on the current state.
  const memberListGenRef = useRef(0)
  const orgFetchGenRef = useRef({})

  // Depend on the primitive identity fields, not the room object reference —
  // a parent re-render that hands us an equivalent-but-new room object
  // shouldn't fire a redundant member.list.
  const fetchMembers = useCallback(async () => {
    if (!account) return
    // Bump the generation before we await so a later invocation (room
    // switch, post-action refetch) marks this one stale by the time its
    // promise resolves. Without this guard, a slow first request that
    // resolves after a second request finishes could overwrite the
    // newer state.
    const gen = ++memberListGenRef.current
    setError(null)
    setLoading(true)
    try {
      const resp = await listRoomMembers(nats, { roomId: room.id, siteId: room.siteId, enrich: true })
      if (gen !== memberListGenRef.current) return
      setMembers(resp.members ?? [])
    } catch (err) {
      if (gen !== memberListGenRef.current) return
      setError(err.message)
    } finally {
      if (gen === memberListGenRef.current) setLoading(false)
    }
  }, [nats, account, room.id, room.siteId])

  useEffect(() => {
    fetchMembers()
  }, [fetchMembers])

  // Owner-status is derived from the just-fetched roster: find the row that
  // matches the current user's account and read isOwner. Memoised so the
  // per-row gating doesn't re-derive on every render.
  const isCurrentUserOwner = useMemo(() => {
    const self = members.find((m) => m.member?.type === 'individual' && m.member?.account === account)
    return !!self?.member?.isOwner
  }, [members, account])

  // Single ordered list: orgs first, individuals second. Within each group
  // the server's enriched-list order is preserved (today: arbitrary; if the
  // server later sorts by sectName / engName the UI inherits that for free).
  const ordered = useMemo(() => {
    const orgs = members.filter((m) => m.member?.type === 'org')
    const individuals = members.filter((m) => m.member?.type === 'individual')
    return [...orgs, ...individuals]
  }, [members])

  /**
   * Wrap an api/<op> call with the shared busy/error/refetch plumbing.
   * Callers pass a thunk that returns the operation's promise; the api
   * layer hides which subject / transport gets used.
   */
  const runAction = useCallback(
    async (key, performer) => {
      setBusyKey(key)
      setActionError(null)
      try {
        await performer()
        await fetchMembers()
        return true
      } catch (err) {
        setActionError(formatAsyncJobError(err))
        return false
      } finally {
        setBusyKey(null)
      }
    },
    [fetchMembers]
  )

  /**
   * Leave the room (member.remove on self). Distinct from the generic
   * runAction path because (a) we don't refetch — we're no longer a member
   * and the call would fail or return an empty roster, and (b) the dialog
   * will dismiss on its own via ChatPage's selectedRoom/summaries effect
   * once subscription.update lands.
   */
  const handleLeave = useCallback(async () => {
    if (!account) return
    if (!window.confirm(`Leave "${room.name}"?`)) return
    setBusyKey(`leave:${account}`)
    setActionError(null)
    try {
      await removeMember(nats, { roomId: room.id, siteId: room.siteId, account })
    } catch (err) {
      setActionError(formatAsyncJobError(err))
    } finally {
      setBusyKey(null)
    }
  }, [room.id, room.siteId, room.name, account, nats])

  /**
   * Toggle an org row open/closed. On the first open we fetch the org's
   * members via the chat.user.{a}.request.orgs.{orgId}.members RPC and
   * cache the result in orgChildren — collapsing then re-expanding doesn't
   * refetch. Loading + error states live in orgFetchState so the row can
   * render a "Loading…" / "Failed to load members" hint without conflating
   * with the top-level dialog error.
   */
  const toggleOrg = useCallback(
    async (orgId) => {
      const wasOpen = !!expandedOrgs[orgId]
      setExpandedOrgs((s) => ({ ...s, [orgId]: !wasOpen }))
      // Collapsing or already cached — nothing else to do.
      if (wasOpen) return
      if (orgChildren[orgId]) return
      if (!account) return
      // Per-org generation guard. If the user expands → collapses →
      // re-expands rapidly, only the latest fetch's resolution writes to
      // state; earlier in-flight responses are dropped on the floor.
      const gen = (orgFetchGenRef.current[orgId] ?? 0) + 1
      orgFetchGenRef.current[orgId] = gen
      setOrgFetchState((s) => ({ ...s, [orgId]: 'loading' }))
      try {
        const resp = await listOrgMembers(nats, { orgId })
        if (orgFetchGenRef.current[orgId] !== gen) return
        setOrgChildren((s) => ({ ...s, [orgId]: resp?.members ?? [] }))
        setOrgFetchState((s) => {
          const { [orgId]: _drop, ...rest } = s
          return rest
        })
      } catch {
        if (orgFetchGenRef.current[orgId] !== gen) return
        setOrgFetchState((s) => ({ ...s, [orgId]: 'error' }))
      }
    },
    [expandedOrgs, orgChildren, nats, account]
  )

  if (loading) return <div className="roster-loading">Loading members…</div>
  if (error) return <div className="dialog-error">{error}</div>

  return (
    <div className="member-roster">
      {actionError && <div className="dialog-error">{actionError}</div>}

      {ordered.length === 0 ? (
        <div className="roster-empty">No members.</div>
      ) : (
        <ul className="roster-list">
          {ordered.map((m) =>
            m.member?.type === 'org'
              ? renderOrgRow(m, {
                  busyKey,
                  isCurrentUserOwner,
                  room,
                  nats,
                  runAction,
                  expanded: !!expandedOrgs[m.member.id],
                  children: orgChildren[m.member.id],
                  fetchState: orgFetchState[m.member.id],
                  toggleOrg,
                })
              : renderIndividualRow(m, {
                  busyKey,
                  isCurrentUserOwner,
                  room,
                  nats,
                  runAction,
                  handleLeave,
                })
          )}
        </ul>
      )}
    </div>
  )
}

function renderOrgRow(m, { busyKey, isCurrentUserOwner, room, nats, runAction, expanded, children, fetchState, toggleOrg }) {
  const entry = m.member
  const orgId = entry.id
  const display = entry.sectName || orgId
  const removeKey = `removeOrg:${orgId}`
  return (
    <li key={m.id} className="roster-row roster-row-org">
      <div className="roster-row-org-header">
        <button
          type="button"
          className="roster-row-info roster-org-toggle"
          aria-expanded={expanded}
          aria-label={`${expanded ? 'Collapse' : 'Expand'} ${display}`}
          onClick={() => toggleOrg(orgId)}
        >
          <span className="roster-chevron" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
          <span className="roster-name">{display}</span>
          {typeof entry.memberCount === 'number' && (
            <span className="roster-secondary">{entry.memberCount} members</span>
          )}
        </button>
        <div className="roster-row-actions">
          {isCurrentUserOwner && (
            <button
              type="button"
              aria-label={`Remove ${orgId}`}
              disabled={busyKey === removeKey}
              onClick={() =>
                runAction(removeKey, () =>
                  removeMember(nats, { roomId: room.id, siteId: room.siteId, orgId })
                )
              }
            >
              Remove
            </button>
          )}
        </div>
      </div>
      {expanded && (
        <ul className="roster-org-children">
          {fetchState === 'loading' && (
            <li className="roster-org-child roster-org-status">Loading members…</li>
          )}
          {fetchState === 'error' && (
            <li className="roster-org-child roster-org-status">Failed to load members.</li>
          )}
          {!fetchState && (children?.length ?? 0) === 0 && (
            <li className="roster-org-child roster-org-status">No members.</li>
          )}
          {children?.map((c) => {
            // Org-children rows show engName + chineseName only — the parent
            // org row already owns the Remove control (server treats the org
            // as a single membership unit; removing it removes all members
            // server-side).
            const primary = c.engName || c.account
            const secondary = c.chineseName || ''
            return (
              <li key={c.id || c.account} className="roster-org-child">
                <span className="roster-name">{primary}</span>
                {secondary && <span className="roster-secondary">{secondary}</span>}
              </li>
            )
          })}
        </ul>
      )}
    </li>
  )
}

function renderIndividualRow(m, { busyKey, isCurrentUserOwner, room, nats, runAction, handleLeave }) {
  const entry = m.member
  const isOwner = !!entry.isOwner
  const isSelf = entry.account === nats.user.account
  const primary = entry.engName || entry.account
  const secondary = entry.chineseName || ''
  const promoteKey = `promote:${entry.account}`
  const demoteKey = `demote:${entry.account}`
  const removeKey = `remove:${entry.account}`
  const leaveKey = `leave:${entry.account}`

  return (
    <li key={m.id} className="roster-row">
      <div className="roster-row-info">
        <span className="roster-name">{primary}</span>
        {secondary && <span className="roster-secondary">{secondary}</span>}
        {isOwner && <span className="roster-badge">owner</span>}
      </div>
      <div className="roster-row-actions">
        {isSelf ? (
          // Self row: only Leave. Remove/Promote/Demote on self would let
          // the user kick themselves or lock themselves out of admin
          // mid-flow — Leave is the one self-applicable action and it
          // confirms first.
          <button
            type="button"
            disabled={busyKey === leaveKey}
            onClick={handleLeave}
          >
            Leave
          </button>
        ) : isCurrentUserOwner ? (
          <>
            {isOwner ? (
              <button
                type="button"
                aria-label={`Demote ${entry.account}`}
                disabled={busyKey === demoteKey}
                onClick={() =>
                  runAction(demoteKey, () =>
                    updateMemberRole(nats, {
                      roomId: room.id,
                      siteId: room.siteId,
                      account: entry.account,
                      newRole: ROLE_MEMBER,
                    })
                  )
                }
              >
                Demote
              </button>
            ) : (
              <button
                type="button"
                aria-label={`Promote ${entry.account}`}
                disabled={busyKey === promoteKey}
                onClick={() =>
                  runAction(promoteKey, () =>
                    updateMemberRole(nats, {
                      roomId: room.id,
                      siteId: room.siteId,
                      account: entry.account,
                      newRole: ROLE_OWNER,
                    })
                  )
                }
              >
                Promote
              </button>
            )}
            <button
              type="button"
              aria-label={`Remove ${entry.account}`}
              disabled={busyKey === removeKey}
              onClick={() =>
                runAction(removeKey, () =>
                  removeMember(nats, {
                    roomId: room.id,
                    siteId: room.siteId,
                    account: entry.account,
                  })
                )
              }
            >
              Remove
            </button>
          </>
        ) : null /* non-owner viewing someone else's row: no controls */}
      </div>
    </li>
  )
}
