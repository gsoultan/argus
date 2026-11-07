//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
)

type otherPackages struct{}

func hostPackagePlatform(string, string) packagePlatform { return otherPackages{} }

const notLinux = "operating system updates from the console are for Linux servers"

func (otherPackages) supported() string { return notLinux }
func (otherPackages) check(context.Context) (PackageReport, error) {
	return PackageReport{Reason: notLinux}, nil
}
func (otherPackages) launch(context.Context, string, string) error { return errors.New(notLinux) }
func (otherPackages) active(string) bool                           { return false }
func (otherPackages) repair(context.Context) error                 { return nil }
func (otherPackages) bootID() string                               { return "" }
