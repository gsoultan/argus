//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
)

// Cleaning logs from the console is for Linux and Windows servers.

const logsNotHere = "cleaning logs from the console is for Linux and Windows servers"

type otherLogs struct{}

func hostLogPlatform(string, string) logPlatform { return otherLogs{} }

func (otherLogs) supported() string { return logsNotHere }
func (otherLogs) scan(context.Context, LogScanRequest) (LogReport, error) {
	return LogReport{Reason: logsNotHere}, nil
}
func (otherLogs) launch(context.Context, string, string) error { return errors.New(logsNotHere) }
func (otherLogs) active(string) bool                           { return false }
func (otherLogs) bootID() string                               { return "" }

func scanHostLogs(context.Context, LogScanRequest) (LogReport, error) {
	return LogReport{Reason: logsNotHere}, nil
}

type otherLogOps struct{}

func hostLogOps(string) logOps { return otherLogOps{} }

func (otherLogOps) protected(string, []string) string { return logsNotHere }
func (otherLogOps) roots(string) []string             { return nil }
func (otherLogOps) real(p string) string              { return p }
func (otherLogOps) journal(context.Context, int, int64) (int64, int64, error) {
	return 0, 0, errors.New(logsNotHere)
}
func (otherLogOps) openTruncate(int, int, string) (int64, error) { return 0, errors.New(logsNotHere) }
func (otherLogOps) restart(context.Context, string) error        { return errors.New(logsNotHere) }
func (otherLogOps) eventLogMax(context.Context, string, int64) (int64, error) {
	return 0, errors.New(logsNotHere)
}
func (otherLogOps) rescan(context.Context, LogPlan) (LogReport, error) {
	return LogReport{Reason: logsNotHere}, nil
}
