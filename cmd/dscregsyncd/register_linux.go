package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func disableTokenEcho(file *os.File) (func() error, error) {
	fd := int(file.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	mode := *original
	mode.Lflag &^= unix.ECHO
	mode.Lflag |= unix.ICANON | unix.ISIG
	mode.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &mode); err != nil {
		return nil, err
	}
	return func() error { return unix.IoctlSetTermios(fd, unix.TCSETS, original) }, nil
}
