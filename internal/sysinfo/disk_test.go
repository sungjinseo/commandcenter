package sysinfo

import (
	"runtime"
	"testing"
)

func TestDiskUsage(t *testing.T) {
	total, free, ok := DiskUsage(t.TempDir())
	if runtime.GOOS != "linux" {
		if ok {
			t.Fatalf("DiskUsage reports ok off Linux")
		}
		return
	}
	if !ok {
		t.Fatal("DiskUsage not ok on Linux")
	}
	if total == 0 {
		t.Fatalf("total = 0")
	}
	if free > total {
		t.Fatalf("free %d > total %d", free, total)
	}
}
