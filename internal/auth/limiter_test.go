package auth

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)

func TestLimiterReserveAndRelease(t *testing.T) {
	l := newLimiter(time.Minute, 2)
	now := t0
	for i := 0; i < 2; i++ {
		if !l.reserve("k", now) {
			t.Fatalf("reserve %d refused", i)
		}
	}
	if l.reserve("k", now) {
		t.Error("third reserve allowed past the limit")
	}
	l.release("k", now)
	if !l.reserve("k", now) {
		t.Error("reserve refused after a release")
	}
	if !l.reserve("other", now) {
		t.Error("another key was refused")
	}
	if !l.reserve("k", now.Add(time.Minute)) {
		t.Error("reserve refused after the window passed")
	}
}

func TestLimiterKeepsAtMostMaxTimestamps(t *testing.T) {
	l := newLimiter(time.Hour, 5)
	for i := 0; i < 100; i++ {
		l.record("k", t0.Add(time.Duration(i)*time.Second))
	}
	last := t0.Add(99 * time.Second)
	if n := len(l.fails["k"]); n != 5 {
		t.Fatalf("kept %d timestamps, want 5", n)
	}
	if got := l.fails["k"][4]; !got.Equal(last) {
		t.Errorf("newest = %v, want %v", got, last)
	}
	if n := l.count("k", last); n != 5 {
		t.Errorf("count = %d, want 5", n)
	}
}

func TestLimiterSweepsExpiredKeys(t *testing.T) {
	l := newLimiter(time.Minute, 5)
	for i := 0; i < 100; i++ {
		l.record(fmt.Sprintf("k%d", i), t0)
	}
	l.record("fresh", t0.Add(30*time.Second))
	l.count("other", t0.Add(61*time.Second))
	if _, ok := l.fails["fresh"]; !ok || len(l.fails) != 1 {
		t.Fatalf("keys after sweep = %d, want only fresh", len(l.fails))
	}
}

func TestLimiterKeyCap(t *testing.T) {
	l := newLimiter(time.Hour, 5)
	l.maxKeys = 3
	for i, k := range []string{"k1", "k2", "k3", "k4"} {
		l.record(k, t0.Add(time.Duration(i)*time.Second))
	}
	if len(l.fails) != 3 {
		t.Fatalf("keys = %d, want 3", len(l.fails))
	}
	if _, ok := l.fails["k1"]; ok {
		t.Error("oldest key was kept")
	}
	if !l.reserve("k5", t0.Add(4*time.Second)) {
		t.Fatal("reserve refused")
	}
	if len(l.fails) != 3 {
		t.Errorf("keys = %d, want 3", len(l.fails))
	}
	if _, ok := l.fails["k2"]; ok {
		t.Error("k2 should have been evicted")
	}
}

func TestLimiterKeyCapSweepsBeforeEvicting(t *testing.T) {
	l := newLimiter(time.Minute, 5)
	l.maxKeys = 3
	l.sweepEvery = 24 * time.Hour
	l.record("k1", t0)
	l.record("k2", t0.Add(time.Second))
	l.record("k3", t0.Add(50*time.Second))
	l.record("k4", t0.Add(65*time.Second))
	if len(l.fails) != 2 {
		t.Fatalf("keys = %d, want 2 (k3, k4)", len(l.fails))
	}
	for _, k := range []string{"k3", "k4"} {
		if _, ok := l.fails[k]; !ok {
			t.Errorf("%s missing", k)
		}
	}
}
