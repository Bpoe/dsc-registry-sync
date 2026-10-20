// Package testutil reproduces the external dscd result-publication handshake in
// tests without importing its internals or executing DSC.
package testutil

import "os"

func ReplaceResult(source, target string) error { return os.Rename(source, target) }
