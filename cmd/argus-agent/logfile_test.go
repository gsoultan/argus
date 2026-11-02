package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A service that runs for months must not grow its log for months.
func TestTheServiceLogStaysBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	const max = 4 << 10
	r, err := openRotating(path, max)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := bytes.Repeat([]byte("x"), 99)
	line = append(line, '\n')
	for i := 0; i < 1000; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > max {
			t.Errorf("%s is %d bytes, over %d", p, fi.Size(), max)
		}
		total += fi.Size()
	}
	if total < max {
		t.Errorf("only %d bytes kept; the newest lines should be there", total)
	}
}

func TestSessionsLogStaysBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	big := make([]byte, maxJSONL)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	appendJSONL(path, map[string]string{"id": "s1"}, nil)
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > 100 {
		t.Fatalf("not started afresh: %v %v", fi.Size(), err)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != maxJSONL {
		t.Fatal("the previous log was not kept as .1")
	}
}
