export const MAX_CACHED = 200

export const BUFFER_MODE = {
  LIVE: 'live',
  HISTORICAL: 'historical',
}

export const initialState = {
  summaries: [],
  roomState: {},
  activeRoomId: null,
  roomsError: null,
}

function sortByLastMsgDesc(summaries) {
  return [...summaries].sort((a, b) => {
    const at = a.lastMsgAt ? new Date(a.lastMsgAt).getTime() : 0
    const bt = b.lastMsgAt ? new Date(b.lastMsgAt).getTime() : 0
    return bt - at
  })
}

function toSummary(room) {
  return {
    id: room.id,
    name: room.name,
    type: room.type,
    siteId: room.siteId,
    userCount: room.userCount,
    lastMsgAt: room.lastMsgAt ?? null,
    unreadCount: 0,
    hasMention: false,
    mentionAll: false,
  }
}

function emptyRoomState() {
  return {
    messages: [],
    hasLoadedHistory: false,
    historyError: null,
    unreadCount: 0,
    hasMention: false,
    mentionAll: false,
    lastMsgAt: null,
    lastMsgId: null,
    bufferMode: BUFFER_MODE.LIVE,
    pendingLiveMessages: [],
    focusMessageId: null,
  }
}

function appendBounded(messages, msg) {
  if (messages.some((m) => m.id === msg.id)) return messages
  const next = [...messages, msg]
  if (next.length > MAX_CACHED) {
    return next.slice(next.length - MAX_CACHED)
  }
  return next
}

export function roomEventsReducer(state, action) {
  switch (action.type) {
    case 'ROOMS_LOADED': {
      const summaries = sortByLastMsgDesc(action.rooms.map(toSummary))
      return { ...state, summaries, roomsError: null }
    }
    case 'ROOM_ADDED': {
      if (state.summaries.some((r) => r.id === action.room.id)) return state
      const summaries = sortByLastMsgDesc([...state.summaries, toSummary(action.room)])
      return { ...state, summaries }
    }
    case 'ROOM_REMOVED': {
      const summaries = state.summaries.filter((r) => r.id !== action.roomId)
      const { [action.roomId]: _removed, ...rest } = state.roomState
      return { ...state, summaries, roomState: rest }
    }
    case 'ROOM_METADATA_UPDATED': {
      const existing = state.summaries.find((r) => r.id === action.roomId)
      if (!existing) return state
      if (
        existing.name === action.name &&
        existing.userCount === action.userCount &&
        existing.lastMsgAt === action.lastMsgAt
      ) {
        return state
      }
      const summaries = sortByLastMsgDesc(
        state.summaries.map((r) =>
          r.id === action.roomId
            ? { ...r, name: action.name, userCount: action.userCount, lastMsgAt: action.lastMsgAt }
            : r
        )
      )
      return { ...state, summaries }
    }
    case 'MESSAGE_RECEIVED': {
      const evt = action.event
      // Channel rooms broadcast encrypted-only events (Message zeroed,
      // EncryptedMessage populated). Without client-side crypto we can't
      // render those, so just skip them rather than crash on the missing
      // .id below. DM rooms always carry a populated .message and proceed
      // normally. The DEV_MODE plaintext fallback in broadcast-worker
      // populates .message for channels too, so dev sees them.
      if (!evt.message || !evt.message.id) {
        return state
      }
      const roomId = evt.roomId
      const prev = state.roomState[roomId] ?? emptyRoomState()
      const isActive = state.activeRoomId === roomId
      if (prev.bufferMode === BUFFER_MODE.HISTORICAL) {
        if (
          prev.messages.some((m) => m.id === evt.message.id) ||
          prev.pendingLiveMessages.some((m) => m.id === evt.message.id)
        ) {
          return state
        }
        const pendingLiveMessages = [...prev.pendingLiveMessages, evt.message]
        const nextRoomState = {
          ...prev,
          pendingLiveMessages,
          lastMsgAt: evt.lastMsgAt ?? prev.lastMsgAt,
          lastMsgId: evt.lastMsgId ?? prev.lastMsgId,
          unreadCount: isActive ? prev.unreadCount : prev.unreadCount + 1,
          hasMention: isActive ? false : prev.hasMention || !!evt.hasMention,
          mentionAll: isActive ? false : prev.mentionAll || !!evt.mentionAll,
        }
        const summaries = state.summaries.some((r) => r.id === roomId)
          ? sortByLastMsgDesc(
              state.summaries.map((r) =>
                r.id === roomId
                  ? {
                      ...r,
                      lastMsgAt: nextRoomState.lastMsgAt ?? r.lastMsgAt,
                      unreadCount: nextRoomState.unreadCount,
                      hasMention: nextRoomState.hasMention,
                      mentionAll: nextRoomState.mentionAll,
                    }
                  : r
              )
            )
          : state.summaries
        return {
          ...state,
          summaries,
          roomState: { ...state.roomState, [roomId]: nextRoomState },
        }
      }
      if (prev.messages.some((m) => m.id === evt.message.id)) return state
      const messages = appendBounded(prev.messages, evt.message)
      const nextRoomState = {
        ...prev,
        messages,
        lastMsgAt: evt.lastMsgAt ?? prev.lastMsgAt,
        lastMsgId: evt.lastMsgId ?? prev.lastMsgId,
        unreadCount: isActive ? prev.unreadCount : prev.unreadCount + 1,
        hasMention: isActive ? false : prev.hasMention || !!evt.hasMention,
        mentionAll: isActive ? false : prev.mentionAll || !!evt.mentionAll,
      }
      const summaries = state.summaries.some((r) => r.id === roomId)
        ? sortByLastMsgDesc(
            state.summaries.map((r) =>
              r.id === roomId
                ? {
                    ...r,
                    lastMsgAt: nextRoomState.lastMsgAt ?? r.lastMsgAt,
                    unreadCount: nextRoomState.unreadCount,
                    hasMention: nextRoomState.hasMention,
                    mentionAll: nextRoomState.mentionAll,
                  }
                : r
            )
          )
        : state.summaries
      return {
        ...state,
        summaries,
        roomState: { ...state.roomState, [roomId]: nextRoomState },
      }
    }
    case 'HISTORY_LOADED': {
      const prev = state.roomState[action.roomId] ?? emptyRoomState()
      const existingIds = new Set(prev.messages.map((m) => m.id))
      const merged = [
        ...action.messages.filter((m) => !existingIds.has(m.id)),
        ...prev.messages,
      ]
      const bounded = merged.length > MAX_CACHED ? merged.slice(merged.length - MAX_CACHED) : merged
      return {
        ...state,
        roomState: {
          ...state.roomState,
          [action.roomId]: {
            ...prev,
            messages: bounded,
            hasLoadedHistory: true,
            historyError: null,
          },
        },
      }
    }
    case 'HISTORY_FAILED': {
      const prev = state.roomState[action.roomId] ?? emptyRoomState()
      return {
        ...state,
        roomState: {
          ...state.roomState,
          [action.roomId]: { ...prev, historyError: action.error },
        },
      }
    }
    case 'REPLACE_ROOM_BUFFER': {
      const prev = state.roomState[action.roomId] ?? emptyRoomState()
      const messages = action.messages ?? []
      return {
        ...state,
        roomState: {
          ...state.roomState,
          [action.roomId]: {
            ...prev,
            messages,
            hasLoadedHistory: true,
            historyError: null,
            bufferMode: BUFFER_MODE.HISTORICAL,
            focusMessageId: action.focusMessageId ?? null,
            pendingLiveMessages: [],
          },
        },
      }
    }
    case 'RESET_TO_LIVE_TAIL': {
      const prev = state.roomState[action.roomId]
      if (!prev) {
        return {
          ...state,
          roomState: {
            ...state.roomState,
            [action.roomId]: emptyRoomState(),
          },
        }
      }
      const existingIds = new Set(prev.messages.map((m) => m.id))
      const newPending = (prev.pendingLiveMessages ?? []).filter(
        (m) => !existingIds.has(m.id)
      )
      const merged = [...prev.messages, ...newPending]
      const bounded =
        merged.length > MAX_CACHED ? merged.slice(merged.length - MAX_CACHED) : merged
      return {
        ...state,
        roomState: {
          ...state.roomState,
          [action.roomId]: {
            ...prev,
            messages: bounded,
            pendingLiveMessages: [],
            focusMessageId: null,
            bufferMode: BUFFER_MODE.LIVE,
          },
        },
      }
    }
    case 'SET_ACTIVE_ROOM': {
      const roomId = action.roomId
      if (roomId === state.activeRoomId) return state
      if (roomId === null) {
        return { ...state, activeRoomId: null }
      }
      const prev = state.roomState[roomId] ?? emptyRoomState()
      const nextRoomState = { ...prev, unreadCount: 0, hasMention: false, mentionAll: false }
      const summaries = state.summaries.map((r) =>
        r.id === roomId ? { ...r, unreadCount: 0, hasMention: false, mentionAll: false } : r
      )
      return {
        ...state,
        activeRoomId: roomId,
        summaries,
        roomState: { ...state.roomState, [roomId]: nextRoomState },
      }
    }
    case 'RESET': {
      return initialState
    }
    case 'ROOMS_FAILED': {
      return { ...state, roomsError: action.error }
    }
    default:
      return state
  }
}
