//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
)

// Growing disks from the console is for Linux and Windows servers.

const disksNotHere = "growing disks from the console is for Linux and Windows servers"

type otherDisks struct{}

func hostDiskPlatform(string, string) diskPlatform { return otherDisks{} }

func (otherDisks) supported() string { return disksNotHere }
func (otherDisks) scan(context.Context, DiskScanRequest) (DiskReport, error) {
	return DiskReport{Reason: disksNotHere}, nil
}
func (otherDisks) launch(context.Context, string, string) error { return errors.New(disksNotHere) }
func (otherDisks) active(string) bool                           { return false }
func (otherDisks) bootID() string                               { return "" }

func scanHostDisks(context.Context, DiskScanRequest) (DiskReport, error) {
	return DiskReport{Reason: disksNotHere}, nil
}

func hostDiskOps() diskOps { return otherDiskOps{} }

type otherDiskOps struct{}

func (otherDiskOps) scan(context.Context, string) (DiskReport, error) {
	return DiskReport{}, errors.New(disksNotHere)
}
func (otherDiskOps) run(context.Context, diskCmd) (string, error) { return "", errors.New(disksNotHere) }
func (otherDiskOps) steps(DiskGrowRequest, GrowPath) ([]diskCmd, error) {
	return nil, errors.New(disksNotHere)
}
func (otherDiskOps) table(context.Context, GrowPath) (diskCmd, bool) { return diskCmd{}, false }
func (otherDiskOps) rescanDisk(GrowPath) string                       { return "" }
