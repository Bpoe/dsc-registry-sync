package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func disableTokenEcho(file *os.File) (func() error, error) {
	handle := windows.Handle(file.Fd())
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		return nil, err
	}
	mode := original &^ windows.ENABLE_ECHO_INPUT
	mode |= windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(handle, mode); err != nil {
		return nil, err
	}
	return func() error { return windows.SetConsoleMode(handle, original) }, nil
}
