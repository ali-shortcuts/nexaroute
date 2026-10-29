package secrets

// File locking for migration and rewrap (ADR 0007 §7.1) lives in
// filelock_linux.go (flock) and filelock_other.go (fail-closed refusal).
// acquireSecretLock takes an exclusive, non-blocking lock on lockPath;
// a second writer fails with a lock message instead of merging with or
// overwriting the first writer's result. The returned release function
// unlocks and closes the lock file; the lock file itself remains on disk
// (empty, mode 0600). Flock releases automatically if the process dies,
// so a crashed operation never leaves a stale lock.
