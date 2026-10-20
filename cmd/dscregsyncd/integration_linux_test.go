package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func verifyPeerProtection(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return file.Close()
	}
	if err == nil {
		err = errors.Join(errors.New("real dscd did not hold exclusive flock"),
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN))
	}
	return errors.Join(err, file.Close())
}

func startPeerProcess(cmd *exec.Cmd) (func() error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}, nil
}
