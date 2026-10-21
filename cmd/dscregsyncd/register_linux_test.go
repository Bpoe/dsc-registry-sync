package main

import (
	"os"
	"testing"
)

func requireRegistrationPrivilege(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root; privileged CI runs onboarding tests")
	}
}
