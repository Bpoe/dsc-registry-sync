package registry

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	minPoll       = 5 * time.Second
	maxPoll       = time.Hour
	maxBackoff    = 5 * time.Minute
	maxRetryAfter = 24 * time.Hour
)

type Schedule struct {
	Fallback time.Duration
	backoff  time.Duration
}

// Next honors server delays before adding up to 10% positive jitter. The caller
// supplies time and a random value in [0,1) to keep policy tests deterministic.
func (s *Schedule) Next(header http.Header, failed bool, now time.Time, random float64) time.Duration {
	delay := cacheDelay(header.Values("Cache-Control"), s.Fallback)
	ceiling := maxPoll
	if failed {
		if s.backoff == 0 {
			s.backoff = minPoll
		} else {
			s.backoff = min(s.backoff*2, maxBackoff)
		}
		delay = max(s.backoff, retryAfter(header.Get("Retry-After"), now))
		ceiling = maxRetryAfter
	} else {
		s.backoff = 0
	}
	return min(delay+time.Duration(float64(delay)*0.1*random), ceiling)
}

func cacheDelay(headers []string, fallback time.Duration) time.Duration {
	delay := fallback
	found, invalid, revalidate := false, false, false
	for _, header := range headers {
		for _, directive := range strings.Split(header, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-cache", "no-store":
				revalidate = true
			case "max-age":
				if found {
					invalid = true
				}
				found = true
				value = strings.TrimSpace(value)
				if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
					value = value[1 : len(value)-1]
				}
				seconds, ok := seconds(value, maxPoll)
				if !ok {
					invalid = true
				} else {
					delay = seconds
				}
			}
		}
	}
	if invalid {
		delay = fallback
	}
	if revalidate {
		delay = minPoll
	}
	return min(max(delay, minPoll), maxPoll)
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if delay, ok := seconds(value, maxRetryAfter); ok {
		return delay
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), maxRetryAfter)
	}
	return 0
}

func seconds(value string, ceiling time.Duration) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n > uint64(ceiling/time.Second) {
		return ceiling, true
	}
	return time.Duration(n) * time.Second, true
}
