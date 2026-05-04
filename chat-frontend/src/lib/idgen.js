// Mirrors pkg/idgen.GenerateMessageID — message-gatekeeper rejects any
// Message.ID that isn't a 20-char base62 string. The previous v4 UUID was
// silently dropped at the gatekeeper and never reached the room.
//
// Algorithm matches the Go side: rejection-sample bytes from crypto random,
// reject ≥ 248, map (b % 62) into the base62 alphabet. This avoids the bias
// you'd get from a naive `% 62` over the full byte range.

const ALPHABET =
  '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'

const MESSAGE_ID_LENGTH = 20

function generateBase62(length) {
  const out = new Array(length)
  let written = 0
  // Slight overdraw to absorb the ~3% rejection rate without re-entering the loop.
  const bufSize = length + Math.ceil(length / 8) + 1
  const buf = new Uint8Array(bufSize)
  while (written < length) {
    crypto.getRandomValues(buf)
    for (let i = 0; i < buf.length && written < length; i++) {
      const b = buf[i]
      if (b >= 248) continue
      out[written++] = ALPHABET[b % 62]
    }
  }
  return out.join('')
}

export function generateMessageID() {
  return generateBase62(MESSAGE_ID_LENGTH)
}
