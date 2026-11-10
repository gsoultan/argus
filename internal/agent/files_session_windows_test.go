package agent

import "testing"

// Paths in a Windows account's files: either slash, ~ for the home folder,
// and a drive's root never something the console offers to delete.
func TestWindowsFilePaths(t *testing.T) {
	f := &fileSession{home: `C:\Users\ana`}
	for in, want := range map[string]string{
		"":                `C:\Users\ana`,
		"~":               `C:\Users\ana`,
		"~/Desktop/a.txt": `C:\Users\ana\Desktop\a.txt`,
		`~\Desktop`:       `C:\Users\ana\Desktop`,
		"Documents":       `C:\Users\ana\Documents`,
		`D:\data`:         `D:\data`,
	} {
		if got, _ := f.resolve(in); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
	for _, p := range []string{`C:\`, `D:\`} {
		if !isRootish(p) {
			t.Errorf("%s is a drive's root and must not be deleted", p)
		}
	}
	if isRootish(`C:\Users\someone-else\x`) {
		t.Error("an ordinary folder was taken for a root")
	}
}
