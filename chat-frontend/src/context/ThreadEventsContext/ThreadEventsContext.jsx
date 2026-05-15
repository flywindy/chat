import { createContext, useCallback, useContext, useEffect, useReducer, useRef } from 'react'
import { v4 as uuidv4 } from 'uuid'
import { useNats } from '../NatsContext/NatsContext'
import { useRoomDispatch } from '../RoomEventsContext/RoomEventsContext'
import { generateMessageID } from '@/lib/idgen'
import { fetchThreadMessages, sendMessage } from '@/api'
import { threadEventsReducer, initialState } from './reducer'

const ThreadEventsContext = createContext(null)

export function ThreadEventsProvider({ children }) {
  const nats = useNats()
  const { user } = nats
  const roomDispatch = useRoomDispatch()
  const [state, dispatch] = useReducer(threadEventsReducer, initialState)
  const generationRef = useRef(0)
  const stateRef = useRef(state)
  stateRef.current = state

  // Reset on logout.
  useEffect(() => {
    if (!user) dispatch({ type: 'RESET' })
  }, [user])

  const openThread = useCallback(
    (parent) => {
      // Short-circuit if it's already the same parent (mirrors reducer guard).
      if (stateRef.current.activeParent?.messageId === parent.messageId) return
      const myGen = ++generationRef.current
      dispatch({ type: 'OPEN_THREAD', parent })
      if (!user) return
      fetchThreadMessages(nats, {
        roomId: parent.roomId,
        siteId: parent.siteId,
        threadMessageId: parent.messageId,
        limit: 50,
      })
        .then((resp) => {
          if (myGen !== generationRef.current) return
          // The api op already normalises into broadcast shape (id/content);
          // hand it straight to the reducer.
          dispatch({ type: 'HISTORY_LOADED', parentId: parent.messageId, resp })
        })
        .catch((err) => {
          if (myGen !== generationRef.current) return
          dispatch({
            type: 'HISTORY_FAILED',
            parentId: parent.messageId,
            error: err?.message ?? String(err),
          })
        })
    },
    [user, nats]
  )

  const closeThread = useCallback(() => {
    generationRef.current++
    dispatch({ type: 'CLOSE_THREAD' })
  }, [])

  // publishReply is synchronous — `publish` is sync void and throws if not
  // connected. Callers wrap in try/catch.
  const publishReply = useCallback(
    (id, content, opts) => {
      const parent = stateRef.current.activeParent
      if (!parent || !user) throw new Error('no active thread')
      const payload = {
        id,
        content,
        requestId: uuidv4(),
        threadParentMessageId: parent.messageId,
        threadParentMessageCreatedAt: parent.createdAtMs,
      }
      if (opts?.quotedParentMessageId) payload.quotedParentMessageId = opts.quotedParentMessageId
      sendMessage(nats, { roomId: parent.roomId, siteId: parent.siteId, payload })
    },
    [user, nats]
  )

  const sendReply = useCallback(
    (content, opts) => {
      const parent = stateRef.current.activeParent
      if (!parent || !user || !content || !content.trim()) return
      const id = generateMessageID()
      const optimistic = {
        id,
        content: content.trim(),
        createdAt: new Date().toISOString(),
        sender: { account: user.account },
        threadParentMessageId: parent.messageId,
        threadParentMessageCreatedAt: new Date(parent.createdAtMs).toISOString(),
        _local: true,
      }
      if (opts?.quotedParentMessageId) {
        // Mirror the server's cassandra.QuotedParentMessage shape so the
        // optimistic snapshot renders identically to what later broadcasts
        // / history loads will carry (messageId / sender.engName / msg).
        const sn = opts.quotedSnapshot?.senderName
        optimistic.quotedParentMessage = {
          messageId: opts.quotedParentMessageId,
          sender: { engName: sn, account: sn },
          msg: opts.quotedSnapshot?.content,
        }
      }
      dispatch({ type: 'REPLY_SENT_LOCAL', message: optimistic })
      try {
        publishReply(id, content.trim(), opts)
        if (parent) {
          roomDispatch({ type: 'OWN_THREAD_REPLY_SENT', roomId: parent.roomId, parentId: parent.messageId })
        }
      } catch (err) {
        dispatch({ type: 'REPLY_SEND_FAILED', messageId: id, error: err?.message ?? String(err) })
      }
    },
    [user, publishReply, roomDispatch]
  )

  const retryReply = useCallback(
    (messageId) => {
      const row = stateRef.current.messages.find((m) => m.id === messageId)
      if (!row) return
      dispatch({ type: 'REPLY_RETRIED', messageId })
      try {
        publishReply(
          messageId,
          row.content,
          row.quotedParentMessage
            ? { quotedParentMessageId: row.quotedParentMessage.messageId ?? row.quotedParentMessage.id }
            : undefined
        )
        // Intentionally NOT dispatching OWN_THREAD_REPLY_SENT here. A retry is
        // the continuation of a logical send, not a new one; double-bumping the
        // parent's tcount would inflate the badge across repeated failures and
        // recoveries. The optimistic count stays at 0 until the next
        // msg.thread reload reconciles from the authoritative server state.
      } catch (err) {
        dispatch({ type: 'REPLY_SEND_FAILED', messageId, error: err?.message ?? String(err) })
      }
    },
    [publishReply]
  )

  const dismissReply = useCallback((messageId) => {
    dispatch({ type: 'REPLY_DISMISSED', messageId })
  }, [])

  const value = {
    activeParent: state.activeParent,
    messages: state.messages,
    hasLoadedHistory: state.hasLoadedHistory,
    historyLoading: state.historyLoading,
    historyError: state.historyError,
    openThread,
    closeThread,
    sendReply,
    retryReply,
    dismissReply,
    dispatch,
  }

  return <ThreadEventsContext.Provider value={value}>{children}</ThreadEventsContext.Provider>
}

export function useThreadEvents() {
  const ctx = useContext(ThreadEventsContext)
  if (!ctx) throw new Error('useThreadEvents must be used inside ThreadEventsProvider')
  return ctx
}
