//go:build darwin

package agent

import "errors"

// newMacVideo would encode motion as H.264 with VideoToolbox.
func newMacVideo(int, int) (screenVideo, error) {
	return nil, errors.New("no H.264 encoder on macOS yet")
}
