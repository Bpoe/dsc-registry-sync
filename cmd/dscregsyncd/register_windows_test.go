package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func requireRegistrationPrivilege(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevation; Windows CI runs onboarding tests as Administrator")
	}
}
