package main

import (
	"errors"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

func verifyPeerProtection(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil
	}
	if err == nil {
		return errors.Join(errors.New("real dscd did not deny write/delete sharing"), windows.CloseHandle(handle))
	}
	return err
}

func startPeerProcess(cmd *exec.Cmd) (func() error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		err = errors.Join(err, windows.CloseHandle(process))
	}
	if err != nil {
		killErr := cmd.Process.Kill()
		_, waitErr := cmd.Process.Wait()
		return nil, errors.Join(err, killErr, waitErr, windows.CloseHandle(job))
	}
	return func() error { return windows.CloseHandle(job) }, nil
}
