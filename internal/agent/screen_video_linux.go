//go:build linux

package agent

import "errors"

// newLinuxVideo would encode motion as H.264. There is no encoder in the
// agent for Linux yet, so motion goes as exact tiles and the agent is told
// why (scrCodec "exact: ...").
func newLinuxVideo(int, int) (screenVideo, error) {
	return nil, errors.New("no H.264 encoder on Linux yet")
}
