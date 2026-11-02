// Package secrets keeps a secret file private.
//
// Every private key Argus loads is checked here before it is read: the
// control plane's CA and TLS keys, the agent's enrollment credential. Both
// binaries need it, so it stays small and shared. The ${VAR} and ${file:...}
// references a control-plane config may hold are resolved by
// internal/control/confexpand, which the agent never links.
package secrets
