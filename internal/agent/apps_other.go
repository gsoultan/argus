//go:build !linux && !darwin

package agent

import "context"

// platformAppScan reads nothing outside Linux yet: there is no /proc to name
// a socket's process from. Containers are still listed where a runtime
// answers on its socket.
func platformAppScan(context.Context) hostScan { return hostScan{} }
