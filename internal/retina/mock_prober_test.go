// Copyright (c) 2026 Sorbonne Université
// SPDX-License-Identifier: MIT

package retina

import (
	"context"
	"testing"
	"time"
)

// runMockProber runs a mock prober until the test ends, and returns its
// channels.
func runMockProber(t *testing.T, config *MockProberConfig) (chan<- PD, <-chan FIE) {
	t.Helper()
	pds := make(chan PD)
	fies := make(chan FIE)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewMockProber(config).Run(ctx, pds, fies) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: got %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return pds, fies
}

func TestMockProber_AnswersInOrderAfterDelay(t *testing.T) {
	delay := 100 * time.Millisecond
	pds, fies := runMockProber(t, &MockProberConfig{Delay: delay, MaxInflight: 8})

	start := time.Now()
	for id := uint32(1); id <= 3; id++ {
		pds <- PD{ID: id}
	}
	for id := uint32(1); id <= 3; id++ {
		fie := <-fies
		if fie.PDID != id || fie.Near != mockNear || fie.Far != mockFar || fie.CaptureUnix == 0 {
			t.Fatalf("FIE %d: got %+v", id, fie)
		}
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("FIEs arrived after %v, before the delay of %v", elapsed, delay)
	}
}

func TestMockProber_NoReply(t *testing.T) {
	pds, fies := runMockProber(t, &MockProberConfig{MaxInflight: 1, NoReply: true})

	pds <- PD{ID: 9}
	if fie := <-fies; fie.PDID != 9 || fie.Near.IsValid() || fie.Far.IsValid() {
		t.Fatalf("got %+v, want no replies", fie)
	}
}

func TestMockProber_StopsTakingPDsAtLimit(t *testing.T) {
	pds, fies := runMockProber(t, &MockProberConfig{Delay: 200 * time.Millisecond, MaxInflight: 2})

	pds <- PD{ID: 1}
	pds <- PD{ID: 2}
	select {
	case pds <- PD{ID: 3}:
		t.Fatal("a third PD was taken while two were in flight")
	case <-time.After(50 * time.Millisecond):
	}

	// Room is made once the FIEs are out. Both are due together, and the
	// prober takes no PD while a FIE is waiting to be written.
	for id := uint32(1); id <= 2; id++ {
		if fie := <-fies; fie.PDID != id {
			t.Fatalf("got FIE %d, want %d", fie.PDID, id)
		}
	}
	select {
	case pds <- PD{ID: 3}:
	case <-time.After(5 * time.Second):
		t.Fatal("no PD was taken after the FIEs were out")
	}
}
