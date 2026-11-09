package cloud

import (
	"context"
	"time"
)

/*
Just-in-time access to a cloud's own console and command line.

Nobody holds standing access to an account's console through Argus. A
person asks for one level for a while, with a reason; where the account's
environment asks for it, someone else agrees; and the grant ends on its own.
While it lasts, each "open the console" or "give me credentials" makes a
fresh session for that person only, with them as the source identity, so
the cloud's own log names them, and never longer than the grant has left.

Sessions are made by chaining from the control plane's identity, which AWS
caps at an hour; a longer grant opens a new session each hour it is used.
*/

// ConsoleLevel is how much a console session may do.
type ConsoleLevel string

const (
	// ConsoleRead sees everything and changes nothing (AWS ReadOnlyAccess).
	ConsoleRead ConsoleLevel = "read"
	// ConsoleOperate changes anything but who may do what (AWS
	// PowerUserAccess: everything except IAM and Organizations).
	ConsoleOperate ConsoleLevel = "operate"
)

// ValidConsoleLevel reports whether l is a level Argus grants.
func ValidConsoleLevel(l ConsoleLevel) bool { return l == ConsoleRead || l == ConsoleOperate }

// MaxConsoleSession is the longest one session lasts.
const MaxConsoleSession = time.Hour

// MinConsoleSession is the shortest session AWS makes.
const MinConsoleSession = 15 * time.Minute

// ConsoleLink is a sign-in link to the cloud's console, good once and for
// a few minutes; the session it opens lasts until Expires.
type ConsoleLink struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

// Credentials are short-lived keys for the command line. They are shown
// once and never stored.
type Credentials struct {
	AccessKeyID     string    `json:"accessKeyId"`
	SecretAccessKey string    `json:"secretAccessKey"`
	SessionToken    string    `json:"sessionToken"`
	Expires         time.Time `json:"expires"`
	Region          string    `json:"region"`
}

// Console is a provider that opens console and command-line sessions as a
// person. a.RoleARN is the role for the level; the provider knows its name.
type Console interface {
	// ConsoleRole is the role a level is in an account, as the account's
	// template makes it.
	ConsoleRole(accountID string, level ConsoleLevel) string
	ConsoleLink(ctx context.Context, a Account, actor Actor, level ConsoleLevel, d time.Duration, issuer, region string) (ConsoleLink, error)
	Credentials(ctx context.Context, a Account, actor Actor, level ConsoleLevel, d time.Duration, region string) (Credentials, error)
}
