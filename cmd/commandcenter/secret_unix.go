//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/sys/unix"
)

// readSecretTTY prompts on /dev/tty and reads one line with echo off. The
// terminal state is restored afterwards, also on Ctrl-C.
func readSecretTTY(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errNoTTY
	}
	defer tty.Close()
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", errNoTTY
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	quiet.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return "", errNoTTY
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
	defer restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	fmt.Fprint(tty, prompt)
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(tty).ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case r := <-got:
		fmt.Fprintln(tty)
		if r.err != nil && r.line == "" {
			return "", errors.New("no password was entered")
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case <-sig:
		fmt.Fprintln(tty)
		return "", errors.New("interrupted")
	}
}
