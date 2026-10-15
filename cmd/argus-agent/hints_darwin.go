//go:build darwin

package main

const (
	startHint      = "sudo launchctl bootstrap system /Library/LaunchDaemons/com.argus.agent.plist"
	reloadSSHDHint = "sudo sshd -t && sudo launchctl kickstart -k system/com.openssh.sshd"
)
