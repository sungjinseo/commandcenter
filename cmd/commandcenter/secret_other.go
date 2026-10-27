//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package main

// readSecretTTY: no terminal support here; NEXUS_PASSWORD is required.
func readSecretTTY(string) (string, error) { return "", errNoTTY }
