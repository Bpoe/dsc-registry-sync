package registry

import (
	"net/http"
	"testing"
	"time"
)

func TestCacheScheduling(t *testing.T) {
	for _, test := range []struct {
		header []string
		want   time.Duration
	}{
		{nil, 300 * time.Second},
		{[]string{"private, max-age=60"}, time.Minute},
		{[]string{`private, MAX-AGE="120"`}, 2 * time.Minute},
		{[]string{"private", "max-age=30"}, 30 * time.Second},
		{[]string{"max-age=0"}, 5 * time.Second},
		{[]string{"max-age=9999999999999999999999999"}, time.Hour},
		{[]string{"max-age=-1"}, 300 * time.Second},
		{[]string{"max-age=nonsense"}, 300 * time.Second},
		{[]string{`max-age="30`}, 300 * time.Second},
		{[]string{"max-age=10", "max-age=20"}, 300 * time.Second},
		{[]string{"private, no-cache, max-age=60"}, 5 * time.Second},
		{[]string{"no-store"}, 5 * time.Second},
	} {
		s := Schedule{Fallback: 300 * time.Second}
		header := http.Header{"Cache-Control": test.header}
		if got := s.Next(header, false, time.Now(), 0); got != test.want {
			t.Errorf("got %v, want %v", got, test.want)
		}
		jittered := s.Next(header, false, time.Now(), 0.75)
		if jittered < test.want || jittered > min(test.want+test.want/10, maxPoll) {
			t.Fatal("jitter violated cache delay bounds")
		}
	}
}

func TestBackoffAndRecovery(t *testing.T) {
	s := Schedule{Fallback: time.Minute}
	now := time.Now()
	for _, seconds := range []int{5, 10, 20, 40, 80, 160, 300, 300, 300} {
		if got := s.Next(nil, true, now, 0); got != time.Duration(seconds)*time.Second {
			t.Errorf("backoff %v, want %ds", got, seconds)
		}
	}
	if got := s.Next(nil, false, now, 0); got != time.Minute {
		t.Fatal("success did not restore normal polling")
	}
	if got := s.Next(nil, true, now, 0); got != 5*time.Second {
		t.Fatal("success did not reset backoff")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 2 * time.Minute},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
		{now.Add(-time.Hour).Format(http.TimeFormat), 5 * time.Second},
		{"-10", 5 * time.Second}, {"invalid", 5 * time.Second}, {"0", 5 * time.Second},
		{"9999999999999999999999999", 24 * time.Hour},
	} {
		s := Schedule{Fallback: time.Minute}
		header := http.Header{"Retry-After": []string{test.value}}
		got := s.Next(header, true, now, 0)
		if got != test.want {
			t.Errorf("got %v, want %v", got, test.want)
		}
	}
}
