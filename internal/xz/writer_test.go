package xz

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"
	"testing/iotest"
)

// TestStoredWriter reads back what the writer wrote, at the sizes around its
// chunk size, with this package's reader and, where it is installed, xz's.
func TestStoredWriter(t *testing.T) {
	xzTool, _ := exec.LookPath("xz")
	for _, n := range []int{0, 1, 3, storedChunk - 1, storedChunk, storedChunk + 1, 3*storedChunk + 7, 1 << 20} {
		want := text(n, uint64(n)+1)
		var out bytes.Buffer
		w := NewStoredWriter(&out)
		// Written in pieces: what is buffered must not depend on them.
		for rest := want; len(rest) > 0; {
			m := min(len(rest), 4099)
			if k, err := w.Write(rest[:m]); err != nil || k != m {
				t.Fatalf("%d: write: %d %v", n, k, err)
			}
			rest = rest[m:]
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%d: close: %v", n, err)
		}
		// the framing, 3 bytes a chunk, as StoredWriter says
		if limit := n + (n/storedChunk+1)*3 + 60; out.Len() > limit {
			t.Fatalf("%d: %d bytes", n, out.Len())
		}
		r, err := NewReader(iotest.OneByteReader(bytes.NewReader(out.Bytes())), Options{})
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%d: read back %d bytes: %v", n, len(got), err)
		}
		if xzTool == "" {
			continue
		}
		cmd := exec.Command(xzTool, "-dc")
		cmd.Stdin = bytes.NewReader(out.Bytes())
		cmd.Stderr = os.Stderr
		if got, err := cmd.Output(); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%d: xz -dc gave %d bytes: %v", n, len(got), err)
		}
	}
}

func TestStoredWriterPassesOnAFailure(t *testing.T) {
	w := NewStoredWriter(failingWriter{})
	if _, err := w.Write(make([]byte, 2*storedChunk)); err != io.ErrClosedPipe {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != io.ErrClosedPipe {
		t.Fatalf("close: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
