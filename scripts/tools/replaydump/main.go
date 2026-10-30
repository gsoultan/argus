// Command replaydump turns a Remote Desktop recording into the replay the
// control plane serves for it, for CI to play back in a browser without an
// object store to fetch it from.
//
//	replaydump <recording.argusrdp> <out>
//
// Prints the replay's info, as the X-Argus-Replay-Info header carries it.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/gsoultan/argus/internal/rdp"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: replaydump <recording> <out>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "replaydump:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	r, err := os.Open(in)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	info, err := rdp.DecodeRecording(r, w)
	if err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(info)
}
