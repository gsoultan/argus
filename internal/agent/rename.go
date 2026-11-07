package agent

import "os"

func renamePlain(from, to string) error { return os.Rename(from, to) }
