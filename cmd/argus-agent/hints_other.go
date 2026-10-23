//go:build !darwin && !windows

package main

const (
	startHint      = "systemctl enable --now argus-agent"
	reloadSSHDHint = "sshd -t && systemctl reload sshd"
)
