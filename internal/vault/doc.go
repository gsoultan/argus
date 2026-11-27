// Package vault is argus-secret: the server that keeps an organization's
// secrets, encrypted under keys that never leave it.
//
// argus-control decides who may read what and records it; argus-secret is the
// only place a value can be opened. The split is the point: a stolen control
// database, or a compromised control process, yields rules and audit entries
// but no secret values. See .serena/memories/argus-secret.md.
//
// Two ways in:
//
//   - The data plane (/v1/...), which agents and app tokens call directly,
//     possibly through Cloudflare Tunnel or another TLS-terminating proxy.
//     Every value leaves sealed to a one-time X25519 key the caller made, so a
//     proxy that reads the body sees only ciphertext.
//   - The management plane (/manage/v1/...), which only argus-control calls.
//     Every request carries an Ed25519 signature by argus-control's pinned
//     key, so it is safe over any transport.
//
// argus-secret decides data-plane reads itself, from the rules argus-control
// last pushed. It refuses every read once those rules are older than
// Config.RulesMaxAge, rather than trust a revocation it may have missed.
//
// Every read, write and refusal is written to a hash-chained log in the same
// database transaction that makes it visible, before the value is returned.
// argus-control collects the log in the background, so a read made while
// argus-control is down still reaches its audit log, and a gap in the chain
// is detectable.
//
// Keys come in three layers: a root key in a file only argus-secret can read
// wraps one key per organization, which wraps one data key per secret version.
package vault
