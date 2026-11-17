// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The fast lane overtakes the slow one; order holds within a lane.
func TestWorkersFastLaneFirst(t *testing.T) {
	w := newWorkers(1, 16, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gate := make(chan struct{})
	var mu sync.Mutex
	var got []string
	var wg sync.WaitGroup
	add := func(s string) func() {
		wg.Add(1)
		return func() { mu.Lock(); got = append(got, s); mu.Unlock(); wg.Done() }
	}
	w.do("d", func() { <-gate }) // hold the worker
	time.Sleep(20 * time.Millisecond)
	w.doSlow("d", add("slow1"))
	w.doSlow("d", add("slow2"))
	w.do("d", add("fast1"))
	w.do("d", add("fast2"))
	close(gate)
	wg.Wait()
	want := []string{"fast1", "fast2", "slow1", "slow2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	for i := 0; i < 100 && w.busy.Load() != 0; i++ {
		time.Sleep(10 * time.Millisecond) // the counter drops after the job returns
	}
	if n := w.busy.Load(); n != 0 {
		t.Fatalf("busy %d after all jobs", n)
	}
}
