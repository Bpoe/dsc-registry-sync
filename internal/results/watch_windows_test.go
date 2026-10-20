package results

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMissingDirectoryUsesWindowsPathCaseSemantics(t *testing.T) {
	requests := make(chan bool, 1)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		requests <- true
	})
	producerRoot := s.root
	s.root = filepath.Join(filepath.Dir(s.root), strings.ToUpper(filepath.Base(s.root)))
	watchers := observeWatchers(s)
	start(t, s)
	first := receive(t, watchers)
	eventually(t, func() bool { return slices.Contains(first.WatchList(), filepath.Dir(s.root)) })
	publishResult(t, &Service{root: producerRoot}, fixture(t, "input-failure"))
	receive(t, requests)
}
