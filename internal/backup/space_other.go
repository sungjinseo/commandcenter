//go:build !unix

package backup

func freeBytes(string) (uint64, bool) { return 0, false }
