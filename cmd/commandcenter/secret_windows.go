package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// readSecretTTY prompts on the console and reads one line with echo off.
func readSecretTTY(prompt string) (string, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return "", errNoTTY
	}
	defer in.Close()
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return "", errNoTTY
	}
	defer out.Close()
	h := windows.Handle(in.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return "", errNoTTY
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT|windows.ENABLE_PROCESSED_INPUT); err != nil {
		return "", errNoTTY
	}
	defer windows.SetConsoleMode(h, mode)
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	fmt.Fprintln(out)
	if err != nil && line == "" {
		return "", errors.New("no password was entered")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
