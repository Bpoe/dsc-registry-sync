package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func requireRegistrationPrivilege(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevation; Windows CI runs onboarding tests as Administrator")
	}
}

func TestRegistrationConsoleCancellation(t *testing.T) {
	if os.Getenv("DSCREGSYNC_TEST_CONSOLE") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRegistrationConsoleCancellation$")
		cmd.Env = append(os.Environ(), "DSCREGSYNC_TEST_CONSOLE=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("console cancellation subprocess failed: %v\n%s", err, output)
		}
		return
	}
	// The private subprocess owns this console and its pending read until exit.
	input, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	handle := windows.Handle(input.Fd())
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sent := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			var mode uint32
			if err := windows.GetConsoleMode(handle, &mode); err != nil {
				sent <- err
				return
			}
			if mode&windows.ENABLE_ECHO_INPUT == 0 {
				// Ctrl+Break maps to os.Interrupt, like Ctrl+C, without targeting
				// any other process outside this subprocess's private console.
				sent <- windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0)
				return
			}
		}
	}()
	data, err := terminalToken(ctx, input)
	if err != context.Canceled || len(data) != 0 {
		t.Fatal("console interrupt did not cancel token input")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	var restored uint32
	if err := windows.GetConsoleMode(handle, &restored); err != nil || restored != original {
		t.Fatal("console interrupt did not restore terminal settings")
	}
	if _, err := readTokenLine(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled input started another read")
	}
}
