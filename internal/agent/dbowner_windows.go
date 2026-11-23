package agent

import "errors"

// hangUp: Windows has no SIGHUP, and Argus finds PgBouncer on Linux only.
func hangUp(int) error { return errors.New("reloading PgBouncer is not supported on Windows") }
