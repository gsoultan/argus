//go:build !windows

package agent

import "testing"

// A stopped agent must not lock the administrators out of the host: root and
// the admin groups get in unrecorded, everyone else is still refused.
func TestBreakGlassLetsAdministratorsInWhileTheRecorderIsDown(t *testing.T) {
	as := func(uid string, groups ...string) func() userInfo {
		return func() userInfo { return userInfo{uid: uid, groups: groups} }
	}
	cases := []struct {
		name   string
		groups []string
		who    func() userInfo
		want   bool
	}{
		{"root", DefaultBreakGlassGroups, as("0"), true},
		{"sudo member", DefaultBreakGlassGroups, as("1000", "users", "sudo"), true},
		{"wheel member", DefaultBreakGlassGroups, as("1000", "wheel"), true},
		{"ordinary user", DefaultBreakGlassGroups, as("1001", "users", "developers"), false},
		{"custom group", []string{"oncall"}, as("1002", "oncall"), true},
		{"custom group, not sudo", []string{"oncall"}, as("1000", "sudo"), false},
		{"turned off, even root", []string{"none"}, as("0"), false},
		{"empty list still admits root", nil, as("0"), true},
	}
	for _, c := range cases {
		if got := breakGlassAllowed(c.groups, c.who); got != c.want {
			t.Errorf("%s: breakGlassAllowed = %v, want %v", c.name, got, c.want)
		}
	}
}
