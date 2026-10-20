package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestBoundedShutdown(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		release := make(chan struct{})
		var output bytes.Buffer
		code := boundedRun(ctx, func() int {
			if blocked {
				<-release
			}
			return 0
		}, &output, 20*time.Millisecond)
		close(release)
		if blocked && (code != 1 || !strings.Contains(output.String(), "cleanup incomplete")) {
			t.Fatal("blocked shutdown was not surfaced")
		}
		if !blocked && code != 0 {
			t.Fatal("clean shutdown failed")
		}
	}
}
