# Quick Share implementation notes

[NearDrop's PROTOCOL.md](https://github.com/grishka/NearDrop/blob/master/PROTOCOL.md) is
the reference and this doesn't repeat it. These are the details that cost time because
they aren't written down there, or are easy to read the wrong way.

## Discovery does not need Bluetooth

For **receiving**, mDNS alone is enough. BLE is only needed for the other direction —
nudging Android into publishing *its* mDNS service so a desktop can send *to* the phone.
rquickshare says this outright, and it's the single most important fact for e-readers,
which have no Bluetooth radio.

A device with no BT can be discovered and sent to over plain wifi.

## Endpoint info encoding

The advertisement's TXT `n=` value, matching rquickshare byte for byte:

```
byte 0     : deviceType << 1        (version 0, visible, reserved bit clear)
bytes 1-16 : 16 random bytes        (salt + encrypted metadata key)
byte 17    : device-name length
bytes 18+  : UTF-8 device name
```

URL-safe base64, **no padding**. Service instance name is
`0x23 | endpointID[4] | 0xFC 0x9F 0x5E | 0x00 0x00`, same encoding.

Note the visibility bit: `0` means visible. A real Quick Share device seen on the wire
had `0x16` (bit 4 set, no name field) — that's a *contacts-only* advertiser, not a
counter-example. Don't copy it.

## UKEY2 details worth getting right

- **The commitment covers the entire `Ukey2Message`**, not just its `message_data`.
  Hashing the payload alone fails the check with nothing to indicate why.
- **Key derivation input:** `IKM = SHA256(ECDH shared secret)`, and
  `info = ClientInit ‖ ServerInit` using the raw bytes exactly as they appeared on the
  wire. Re-serializing the protobufs can produce different bytes and silently break the
  derivation.
- **EC coordinates use Java `BigInteger` encoding** — signed big-endian, so a leading
  `0x00` is prepended when the top bit is set. Emit that on the way out, and strip it and
  re-pad to 32 bytes on the way in.
- Salts, in order: `"UKEY2 v1 auth"` and `"UKEY2 v1 next"`, then the two fixed D2D salts
  with info strings `client`/`server`, then `ENC:2`/`SIG:1`.

A mistake anywhere here surfaces much later as an HMAC mismatch on the first encrypted
frame, with no hint about which step was wrong. Verify the handshake completes before
debugging anything above it.

## The connection sequence

```
client -> ConnectionRequest      (plaintext OfflineFrame)
client -> Ukey2ClientInit
server -> Ukey2ServerInit
client -> Ukey2ClientFinish
both   -> ConnectionResponse     (plaintext, ACCEPT)
--- everything below is encrypted ---
client -> PairedKeyEncryption    server -> PairedKeyEncryption
client -> PairedKeyResult        server -> PairedKeyResult (UNABLE)
client -> Introduction           server -> RESPONSE (ACCEPT)
client -> PAYLOAD_TRANSFER (FILE) chunks
server -> PAYLOAD_ACK
server -> DISCONNECTION
```

**Both sides must send `ConnectionResponse`.** The phone sends its own immediately and
then sits in a keep-alive loop; encryption doesn't begin until ours arrives. It looks
exactly like a hung handshake.

**`PAYLOAD_ACK` and `DISCONNECTION` are not optional.** Skip them and the file arrives
intact while the sender's UI never completes.

## Framing

Every message is preceded by a 4-byte big-endian length. Encrypted frames are:

```
SecureMessage{
  header_and_body = HeaderAndBody{
      header = Header{ sig=HMAC_SHA256, enc=AES_256_CBC, iv, public_metadata },
      body   = AES-256-CBC( DeviceToDeviceMessage{ seq, message } ),
  }
  signature = HMAC-SHA256(header_and_body)
}
```

`message` is a serialized `OfflineFrame`. Encrypt-then-MAC: **verify the HMAC before
parsing anything inside**, or you're feeding attacker-controlled bytes to a protobuf
parser.

Payloads are chunked. `BYTES` payloads reassemble into a `sharing.Frame` — the protocol
proper — and `FILE` payloads are file contents. A payload isn't complete until a chunk
arrives with `flags & 1` (LAST_CHUNK); for outgoing frames that's a zero-length chunk at
the end, which peers wait for before acting.

## Things that are not what they look like

- Keep-alives and the peer's `ConnectionResponse` can still arrive as **plaintext** after
  the handshake. Fall back to parsing them as plain `OfflineFrame` rather than treating a
  decryption failure as fatal.
- `BANDWIDTH_UPGRADE_RETRY` and `PROGRESS_UPDATE` frames appear mid-transfer and can be
  ignored safely.
- Android randomizes its MAC per network, so the peer's address changes between
  transfers. Don't key anything on it.
