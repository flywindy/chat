import { connect, type NatsConnection, type Msg } from "nats.ws";

// ---- Types matching Go models ----

interface RoomKeyEvent {
  roomId: string;
  version: number;
  privateKey: string; // base64-encoded 32-byte room secret, used directly as AES-256 key
}

interface EncryptedMessage {
  version: number;
  nonce: string;      // base64-encoded 12-byte AES-GCM nonce
  ciphertext: string; // base64-encoded ciphertext + 16-byte GCM tag
}

// ---- Decryption (matches pkg/roomcrypto direct-import scheme) ----

async function decryptMessage(
  encrypted: EncryptedMessage,
  roomPrivateKeyB64: string,
): Promise<string> {
  const privKeyBytes = Buffer.from(roomPrivateKeyB64, "base64");

  // Import room private key directly as AES-256-GCM key (no HKDF step).
  const aesKey = await crypto.subtle.importKey(
    "raw",
    privKeyBytes,
    { name: "AES-GCM", length: 256 },
    false,
    ["decrypt"],
  );

  // AES-256-GCM decrypt (ciphertext includes the 16-byte GCM tag appended).
  const nonce = Buffer.from(encrypted.nonce, "base64");
  const ciphertext = Buffer.from(encrypted.ciphertext, "base64");

  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: nonce },
    aesKey,
    ciphertext,
  );

  return new TextDecoder().decode(plaintext);
}

// ---- Main ----

async function main(): Promise<void> {
  const [natsURL, account, roomID] = process.argv.slice(2);
  if (!natsURL || !account || !roomID) {
    process.stderr.write("usage: tsx client.ts <nats-ws-url> <account> <roomID>\n");
    process.exit(1);
  }

  const keySubject = `chat.user.${account}.event.room.key`;
  const msgSubject = `test.room.${roomID}.msg`;

  // Store received keys indexed by version number.
  const keys = new Map<number, string>();

  const nc: NatsConnection = await connect({ servers: natsURL });

  // Subscribe to key events.
  const keySub = nc.subscribe(keySubject);
  // Subscribe to encrypted messages.
  const msgSub = nc.subscribe(msgSubject);

  // Process key events in background.
  (async () => {
    for await (const msg of keySub) {
      const evt: RoomKeyEvent = JSON.parse(new TextDecoder().decode(msg.data));
      keys.set(evt.version, evt.privateKey);
    }
  })();

  // Process encrypted messages — decrypt first one and exit.
  for await (const msg of msgSub) {
    const versionStr = msg.headers?.get("X-Room-Key-Version");
    if (!versionStr) {
      // No header means the message is not encrypted — print raw and exit.
      process.stdout.write(new TextDecoder().decode(msg.data));
      break;
    }

    const version = parseInt(versionStr, 10);
    const privateKey = keys.get(version);
    if (!privateKey) {
      process.stderr.write(`no key found for version ${version}\n`);
      process.exit(1);
    }

    const encrypted: EncryptedMessage = JSON.parse(new TextDecoder().decode(msg.data));
    const plaintext = await decryptMessage(encrypted, privateKey);

    process.stdout.write(plaintext);
    break;
  }

  await nc.drain();
}

main().catch((err: unknown) => {
  process.stderr.write(`error: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
