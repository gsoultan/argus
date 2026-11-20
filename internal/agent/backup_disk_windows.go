package agent

func freeBytes(string) int64 { return 0 }

// ownLike is nothing on Windows: the file inherits the folder's ACL.
func ownLike(string, string) error { return nil }
