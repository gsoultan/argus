package agent

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// platformMachineID is the GUID Windows generates at install, which survives
// a rename the way /etc/machine-id does on Linux.
func platformMachineID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	id, _, _ := k.GetStringValue("MachineGuid")
	return id
}

// platformOSName is "Windows Server 2022 Datacenter 21H2".
func platformOSName() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	ver, _, _ := k.GetStringValue("DisplayVersion")
	return strings.TrimSpace(name + " " + ver)
}

const profileList = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`

// profileDirOf is a logged-on account's profile directory, from the
// registry; "" if it has none yet.
func profileDirOf(sid string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList+`\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	dir, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return ""
	}
	if exp, err := registry.ExpandString(dir); err == nil {
		return exp
	}
	return dir
}

// profilesRoot is where new profiles are created, normally C:\Users.
func profilesRoot() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if dir, _, err := k.GetStringValue("ProfilesDirectory"); err == nil {
			if exp, err := registry.ExpandString(dir); err == nil {
				return exp
			}
		}
	}
	return `C:\Users`
}
