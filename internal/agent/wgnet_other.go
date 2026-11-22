//go:build !linux

package agent

import "context"

type otherWGNets struct{}

func hostWGNetPlatform(string) wgnetPlatform { return otherWGNets{} }

func (otherWGNets) supported() string                            { return errWGNetNotHere.Error() }
func (otherWGNets) launch(context.Context, string, string) error { return errWGNetNotHere }
func (otherWGNets) active(string) bool                           { return false }

func hostWGNetOps() (wgnetOps, error) { return nil, errWGNetNotHere }
