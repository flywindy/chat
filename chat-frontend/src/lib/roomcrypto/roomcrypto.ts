/**
 * Decode a standard-base64 string to a Uint8Array.
 *
 * Note: this is base64, not base64url. The server emits standard base64
 * via Go's encoding/json default for []byte fields (StdEncoding).
 */
export function b64decode(s: string): Uint8Array {
  const binary = atob(s)
  const out = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i)
  return out
}

/**
 * Import a 32-byte room private key as a non-extractable AES-256-GCM
 * CryptoKey for decryption. The room secret is uniform random material
 * and is used directly as the AES-256 key without any key derivation step.
 *
 * The returned key is non-extractable and has the single usage 'decrypt'.
 */
export async function importAesKey(roomPrivateKey: Uint8Array): Promise<CryptoKey> {
  if (roomPrivateKey.length !== 32) {
    throw new Error(`room private key must be 32 bytes, got ${roomPrivateKey.length}`)
  }
  return crypto.subtle.importKey(
    'raw',
    roomPrivateKey.buffer.slice(roomPrivateKey.byteOffset, roomPrivateKey.byteOffset + roomPrivateKey.byteLength) as ArrayBuffer,
    { name: 'AES-GCM', length: 256 },
    false,
    ['decrypt'],
  )
}

/**
 * Decrypt a server-produced {nonce, ciphertext} pair using the AES key
 * imported via importAesKey. The ciphertext is body || 16-byte GCM tag,
 * matching Go's cipher.AEAD.Seal output.
 */
export async function decryptRoomMessage(
  ciphertext: Uint8Array,
  nonce: Uint8Array,
  aesKey: CryptoKey,
): Promise<string> {
  if (nonce.length !== 12) {
    throw new Error(`nonce must be 12 bytes, got ${nonce.length}`)
  }
  const ivBuffer = nonce.buffer.slice(nonce.byteOffset, nonce.byteOffset + nonce.byteLength) as ArrayBuffer
  const ctBuffer = ciphertext.buffer.slice(ciphertext.byteOffset, ciphertext.byteOffset + ciphertext.byteLength) as ArrayBuffer
  const plaintext = await crypto.subtle.decrypt(
    { name: 'AES-GCM', iv: ivBuffer, tagLength: 128 },
    aesKey,
    ctBuffer,
  )
  return new TextDecoder('utf-8').decode(plaintext)
}
