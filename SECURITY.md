# Security

The canonical security model, encrypted-secret storage, TLS/mTLS, CSRF, operational procedures, and documented limitations are maintained in [docs/SECURITY.md](docs/SECURITY.md). Keep security claims in that document to avoid conflicting or duplicated guidance.

The optional Vault Transit `nxs2` envelope mode and its explicit opt-in, migration, rewrap, and fail-closed boundaries are documented in [docs/SECURITY.md](docs/SECURITY.md#vault-transit-envelope-mode-nxs2). It is fake-API tested but has no real Vault Transit acceptance evidence yet.
