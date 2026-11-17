// Package staging stores a migration run's intermediate data: the uploaded
// Slack export and the batch files each stage produces.
//
// # Threat model
//
// The object store is untrusted. Whoever operates it (or steals its disks)
// can read, replace, truncate, reorder, rename and delete objects. Staging
// therefore encrypts and authenticates every object's contents in the
// client. What the store still learns: object key names, approximate
// object sizes, and when objects are written. Key names are NOT encrypted, so
// they must never contain sensitive data; use the RunPrefix, ExportKey and
// BatchKey helpers, which only embed identifiers.
//
// # Layers
//
//   - Blobs is a minimal object-store interface with two backends: NewFS (a
//     local directory) and NewS3 (any S3-compatible store). Neither encrypts.
//   - Encrypted wraps any Blobs with streaming authenticated encryption under
//     a per-run data key (DataKey). Production code must only ever talk to
//     the Encrypted view.
//   - KEK is the operator-supplied key-encryption key. It wraps the DataKey
//     so the wrapped form can be stored next to the run's state. Purging a
//     run is deleting its objects plus forgetting the wrapped data key.
//
// # Object format (TAUTENC1)
//
// The format is the STREAM construction of Hoang, Reyhanitabar, Rogaway and
// Vizár ("Online Authenticated-Encryption and its Nonce-Reuse
// Misuse-Resistance", CRYPTO 2015), the same scheme used by Tink's streaming
// AEAD and by age. All integers are big-endian.
//
//	header  (28 bytes) = "TAUTENC1" | salt (16 random bytes) | chunkSize (uint32)
//	okm     (39 bytes) = HKDF-SHA256(secret = data key, salt = salt,
//	                                 info = "taut-staging-stream-v1")
//	subkey             = okm[0:32]      AES-256-GCM key for this object only
//	prefix             = okm[32:39]     7-byte nonce prefix
//	nonce   (12 bytes) = prefix | counter (uint32) | final (0x00 or 0x01)
//	aad                = header | object key name (UTF-8 bytes)
//	chunk i            = AES-256-GCM(subkey, nonce(i, final), plaintext_i, aad)
//	                     = ciphertext_i | 16-byte tag
//	object             = header | chunk 0 | chunk 1 | … | chunk n-1
//
// Every chunk except the last holds exactly chunkSize plaintext bytes
// (65536 for objects this package writes). The last chunk holds between 1 and
// chunkSize bytes and is sealed with final = 0x01; an empty object is a
// single empty final chunk, and that is the only case in which a chunk may
// be empty.
//
// Why each piece is there:
//
//   - A fresh random salt gives every object (and every overwrite of the same
//     key) its own AES key, so GCM nonces never repeat under one key however
//     many objects a run writes.
//   - The counter in the nonce pins each chunk to its position: reordered,
//     duplicated or dropped chunks fail authentication.
//   - The final flag in the nonce pins the end of the stream: a truncated
//     object ends in a chunk that was not sealed as final, and an object with
//     data appended ends in a chunk that does not authenticate.
//   - The header in the AAD authenticates the chunk size (the salt is already
//     bound through the key derivation).
//   - The object key name in the AAD means a ciphertext copied or renamed to
//     another key fails to decrypt, so the store cannot swap two objects.
//
// A reader authenticates each chunk before releasing any of its plaintext
// and reports io.EOF only after it has authenticated the final chunk. Like
// every streaming AEAD it releases earlier chunks before it has seen later
// ones: a consumer must treat a read error as "discard everything read so
// far", never as a short but usable object.
//
// Limits: 2^32 chunks per object (256 TiB at the default chunk size);
// exceeding it is an error, never a wrapped counter.
//
// # Wrapped data key format (taut-kek-v1)
//
//	wrapped (71 bytes) = "taut-kek-v1" | nonce (12 random bytes) |
//	                     AES-256-GCM(KEK, nonce, data key (32 bytes),
//	                                 aad = "taut-kek-v1")
package staging
