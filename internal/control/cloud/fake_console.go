package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The fake as Console: sessions are recorded with who and how long, and
// say so in what they return.

func (f *Fake) ConsoleRole(accountID string, level ConsoleLevel) string {
	if level == ConsoleOperate {
		return "arn:aws:iam::" + accountID + ":role/ArgusConsoleOperate"
	}
	return "arn:aws:iam::" + accountID + ":role/ArgusConsoleRead"
}

func (f *Fake) consoleCall(a Account, actor Actor, what string, level ConsoleLevel, d time.Duration) error {
	f.record(fmt.Sprintf("%s %s %s %s", what, level, actor, d))
	if a.ExternalID == "" {
		return errors.New("AccessDenied: no external id")
	}
	if d < MinConsoleSession || d > MaxConsoleSession {
		return fmt.Errorf("ValidationError: DurationSeconds %d is out of range", int(d.Seconds()))
	}
	return nil
}

func (f *Fake) ConsoleLink(_ context.Context, a Account, actor Actor, level ConsoleLevel, d time.Duration, issuer, region string) (ConsoleLink, error) {
	if err := f.consoleCall(a, actor, "console", level, d); err != nil {
		return ConsoleLink{}, err
	}
	return ConsoleLink{URL: "https://signin.fake/federation?Action=login&Issuer=" + issuer + "&region=" + region, Expires: time.Now().Add(d)}, nil
}

func (f *Fake) Credentials(_ context.Context, a Account, actor Actor, level ConsoleLevel, d time.Duration, region string) (Credentials, error) {
	if err := f.consoleCall(a, actor, "credentials", level, d); err != nil {
		return Credentials{}, err
	}
	return Credentials{AccessKeyID: "ASIAFAKE", SecretAccessKey: "secret", SessionToken: "token", Expires: time.Now().Add(d), Region: region}, nil
}

var _ Console = (*Fake)(nil)
