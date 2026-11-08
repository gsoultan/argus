package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upload is only written when its closing frame arrives: a stream cut
// short, even exactly between frames, leaves nothing behind.
func TestAFramedUploadIsWrittenOnlyWhenComplete(t *testing.T) {
	frames := func(chunks ...[]byte) *bytes.Buffer {
		var b bytes.Buffer
		for _, c := range chunks {
			_ = writeUploadFrame(&b, c)
		}
		return &b
	}
	dir := t.TempDir()

	whole := filepath.Join(dir, "whole.txt")
	if err := writeFile(whole, &uploadFrames{r: frames([]byte("hello "), []byte("world"), nil)}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(whole); string(got) != "hello world" {
		t.Fatalf("wrote %q", got)
	}

	for name, in := range map[string]*bytes.Buffer{
		"between frames": frames([]byte("hello "), []byte("world")),
		"mid frame":      bytes.NewBuffer(frames([]byte("hello world")).Bytes()[:7]),
		"nothing":        frames(),
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := writeFile(path, &uploadFrames{r: in}, false); err == nil {
			t.Errorf("%s: a cut upload was written", name)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: left %s behind", name, path)
		}
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 {
		t.Errorf("temporary files left behind: %d entries", len(left))
	}
}
