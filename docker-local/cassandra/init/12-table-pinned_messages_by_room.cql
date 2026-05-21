CREATE TABLE IF NOT EXISTS chat.pinned_messages_by_room (
  room_id               TEXT,
  created_at            TIMESTAMP, // = pinnedAt
  message_id            TEXT,
  sender                FROZEN<"Participant">,
  msg                   TEXT,
  mentions              SET<FROZEN<"Participant">>,
  attachments           LIST<BLOB>,
  file                  FROZEN<"File">,
  card                  FROZEN<"Card">,
  card_action           FROZEN<"CardAction">,
  quoted_parent_message FROZEN<"QuotedParentMessage">,
  visible_to            TEXT,
  reactions             MAP<TEXT, FROZEN<SET<FROZEN<"Participant">>>>,
  deleted               BOOLEAN,
  type                  TEXT,
  sys_msg_data          BLOB,
  site_id               TEXT,
  edited_at             TIMESTAMP,
  updated_at            TIMESTAMP,
  pinned_by             FROZEN<"Participant">,
  PRIMARY KEY ((room_id), created_at, message_id)
) WITH CLUSTERING ORDER BY (created_at DESC, message_id DESC);
