package auth

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// limiterMaxKeys bounds each limiter's map. When it is full, expired keys
	// are swept first, then the key with the oldest newest failure is dropped.
	limiterMaxKeys = 10000
	// limiterSweepEvery is how often a limiter drops every expired key. The
	// sweep runs inside the next call after the interval, so no goroutine.
	limiterSweepEvery = time.Minute
	// maxRawIPKeyLen caps an IP string that does not parse.
	maxRawIPKeyLen = 64
)

// limiter counts recent failures per key in a sliding window. It keeps at
// most max timestamps per key (the newest), because counts at or above max
// all mean the same thing to its callers.
type limiter struct {
	mu         sync.Mutex
	window     time.Duration
	max        int
	maxKeys    int
	sweepEvery time.Duration
	lastSweep  time.Time
	fails      map[string][]time.Time
}

func newLimiter(window time.Duration, max int) *limiter {
	return &limiter{window: window, max: max, maxKeys: limiterMaxKeys, sweepEvery: limiterSweepEvery, fails: make(map[string][]time.Time)}
}

// count returns key's failures in the window, at most max.
func (l *limiter) count(key string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweep(now)
	return len(l.prune(key, now))
}

// blocked reports whether key has used up its failures. It records nothing.
func (l *limiter) blocked(key string, now time.Time) bool { return l.count(key, now) >= l.max }

// reserve records an attempt for key unless key has used up its failures,
// and reports whether the attempt may go ahead. The reservation counts as a
// failure until release removes it.
func (l *limiter) reserve(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweep(now)
	if len(l.prune(key, now)) >= l.max {
		return false
	}
	l.add(key, now)
	return true
}

// record counts an attempt for key unconditionally. Like a reservation it
// counts as a failure until release removes it.
func (l *limiter) record(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweep(now)
	l.prune(key, now)
	l.add(key, now)
}

// release removes one reservation made at t for key, for an attempt that
// turned out not to be a failure.
func (l *limiter) release(key string, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fails := l.fails[key]
	for i := len(fails) - 1; i >= 0; i-- {
		if fails[i].Equal(t) {
			fails = append(fails[:i], fails[i+1:]...)
			break
		}
	}
	if len(fails) == 0 {
		delete(l.fails, key)
		return
	}
	l.fails[key] = fails
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// prune drops expired entries for key. Callers hold l.mu.
func (l *limiter) prune(key string, now time.Time) []time.Time {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

// add appends now to key, dropping the oldest timestamp once key holds max,
// and makes room when key is new and the map is full. Callers hold l.mu and
// have pruned key.
func (l *limiter) add(key string, now time.Time) {
	ts, ok := l.fails[key]
	if !ok && len(l.fails) >= l.maxKeys {
		l.sweep(now)
		if len(l.fails) >= l.maxKeys {
			l.evictOldest()
		}
	}
	if len(ts) >= l.max {
		copy(ts, ts[1:])
		ts[len(ts)-1] = now
	} else {
		ts = append(ts, now)
	}
	l.fails[key] = ts
}

// maybeSweep sweeps when sweepEvery has passed since the last sweep. Callers hold l.mu.
func (l *limiter) maybeSweep(now time.Time) {
	if now.Sub(l.lastSweep) >= l.sweepEvery {
		l.sweep(now)
	}
}

// sweep drops every expired timestamp and empty key. Callers hold l.mu.
func (l *limiter) sweep(now time.Time) {
	for k := range l.fails {
		l.prune(k, now)
	}
	l.lastSweep = now
}

// evictOldest drops the key whose newest failure is oldest. Callers hold l.mu.
func (l *limiter) evictOldest() {
	var (
		oldestKey string
		oldest    time.Time
		found     bool
	)
	for k, ts := range l.fails {
		if last := ts[len(ts)-1]; !found || last.Before(oldest) {
			oldestKey, oldest, found = k, last, true
		}
	}
	if found {
		delete(l.fails, oldestKey)
	}
}

// ipKey is the limiter key for a client address: an IPv4 address as is
// (IPv4-mapped IPv6 is unmapped), an IPv6 address as its /64, zones dropped.
// A string that does not parse is used as is, cut to maxRawIPKeyLen bytes.
func ipKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		if len(ip) > maxRawIPKeyLen {
			return ip[:maxRawIPKeyLen]
		}
		return ip
	}
	a = a.WithZone("").Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}
