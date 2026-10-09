# Secret encryption at rest (Phase 1a)

NexaRoute stores secret-bearing configuration values as versioned AES-256-GCM envelopes (`nxs1:<keyid>:<nonce>:<ciphertext>`). Each secret gets a fresh random 256-bit data-encryption key; that key is wrapped with the master key. Associated data binds the ciphertext to its provider identifier and exact field (including credential-pool index and header name), preventing moving a value to another field. Provider API keys, credential pool API keys, decision-provider keys, admin API key, client-auth keys, provider custom header values, and proxy URLs are covered. Environment-variable references remain references, not secret values.

## Master key selection

The sources are checked in this order:

1. `NEXAROUTE_MASTER_KEY`: standard base64 encoding of exactly 32 bytes. This is convenient for secret injection but the process environment and deployment system become part of the key trust boundary. Environment values override a configured key file.
2. `NEXAROUTE_MASTER_KEY_FILE`: a path to a 32-byte raw key file. Existing files must have exactly mode `0600`; looser modes are rejected with a repair instruction. This gives operators control of key placement/backups, but the key file must be secured and backed up independently.
3. If neither is set, NexaRoute creates `<config-path>.key` on first use with mode `0600`. This preserves zero-setup default installs and keeps the key beside the config. It protects against disclosure of the config file alone, **not** theft of the whole directory, a compromised host, root access, or storage snapshots containing both files. Protect and back up the key separately.

There is no OS keyring integration in this change; that remains future work. Do not delete, regenerate, or overwrite the master key as a recovery technique.

## Migration and operations

On load, plaintext legacy secret fields are encrypted and the config is replaced using a same-directory temporary file, file `fsync`, rename, and directory sync. Before replacing the config, a timestamped `.enc.bak` is written; it is an authenticated encrypted blob, not a plaintext copy. Migration is idempotent. Errors loading ciphertext, mismatched AAD, or missing/wrong keys fail closed: the gateway does not start with partially decrypted configuration. Fix the key source or restore the matching key.

- `nexaroute secrets status --config PATH` prints ciphertext counts only.
- `nexaroute secrets verify --config PATH` decrypts in memory and prints field identifiers plus OK/FAIL, never values.
- `nexaroute secrets rotate --config PATH` re-encrypts secrets with a newly generated key when using the automatically managed key file. A mode-0600 `.key.previous` recovery copy is retained. Rotation is unavailable when master-key environment overrides are configured; arrange key custody before changing those sources.
- `nexaroute secrets decrypt --config PATH --to-stdout --allow-plaintext` explicitly emits a plaintext JSON config to stdout. Treat terminal capture, shell pipes, and redirected output as sensitive. Both flags are required.

The config and master-key file are separate filesystem objects, so rotation cannot provide a cross-file atomic transaction on all filesystems. The retained previous key is a recovery aid if key activation is interrupted; restore the key matching the current config before restarting. Protect and remove the previous-key copy only after verifying the new key and keeping a secure offline recovery copy.

## Recovery and limitations

**Loss of the only matching master key means the encrypted secrets are unrecoverable.** Restore the exact key from an independently protected backup, or restore an encrypted backup and its matching key. If neither exists, the operator must obtain and enter replacement credentials; there is no bypass, plaintext fallback, or recovery secret. Key rotation is not a substitute for tested key backup and restore. Client virtual keys already stored as one-way hashes cannot be recovered from those hashes.

Admin reads remain write-only and do not return ciphertext or plaintext secret fields. Keep the admin surface on trusted networks. Encryption at rest does not replace host security, access control, TLS, or encrypted backups of the full machine.
