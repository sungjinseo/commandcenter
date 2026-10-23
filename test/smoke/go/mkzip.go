// Command mkzip writes a Go module zip: every file under dir is stored as
// "<prefix>/<relative path>". Usage: go run mkzip.go DIR MODULE@VERSION OUT.zip
package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

func main() {
	dir, prefix, out := os.Args[1], os.Args[2], os.Args[3]
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	zw := zip.NewWriter(f)
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(prefix + "/" + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}
