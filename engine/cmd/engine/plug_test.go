// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"testing"
	"time"
)

// One plug-in reported three times counts once; an unplug ends the window.
func TestPlugCountedOnce(t *testing.T) {
	ctx := context.Background()
	check := func(e *engine, dev string) {
		if rep, _ := e.plugRepeated(ctx, dev, "b1-1"); rep {
			t.Fatal("first plugged taken for a repeat")
		}
		if rep, _ := e.plugRepeated(ctx, dev, "b1-1"); !rep {
			t.Fatal("repeated plugged counted again")
		}
		if rep, _ := e.plugRepeated(ctx, dev, "b1-1"); !rep {
			t.Fatal("repeated plugged counted again")
		}
		e.plugForget(ctx, dev)
		if rep, prev := e.plugRepeated(ctx, dev, "b1-2"); rep || prev != "" {
			t.Fatalf("a plug-in after an unplug: repeat %v, supersedes %q", rep, prev)
		}
		e.plugForget(ctx, dev)
		// a device before 0.3.2: no ids, the window alone
		if rep, _ := e.plugRepeated(ctx, dev, ""); rep {
			t.Fatal("first plugged (no id) taken for a repeat")
		}
		if rep, _ := e.plugRepeated(ctx, dev, ""); !rep {
			t.Fatal("repeated plugged (no id) counted again")
		}
		e.plugForget(ctx, dev)
	}
	check(&engine{st: newMemState()}, "t-tp-plug-mem") // tests without Valkey: in memory
	v := testValkey(t)                                 // the engines: shared in Valkey
	check(&engine{st: v}, "t-tp-plug-vk")
}

// Issue #15: a quick replug. The plug-in id changes within the window: with
// an unplug in between (100 ms) and without one (the device counted a new
// plug-in, its "unplugged" never came, 0.7 s). Both are new plug-ins; the
// second supersedes the first.
func TestPlugQuickReplug(t *testing.T) {
	ctx := context.Background()
	check := func(e *engine, dev string) {
		defer e.plugForget(ctx, dev)
		if rep, prev := e.plugRepeated(ctx, dev, "ccd6a0a9-1"); rep || prev != "" {
			t.Fatalf("plug-in -1: repeat %v, supersedes %q", rep, prev)
		}
		time.Sleep(100 * time.Millisecond)
		e.plugForget(ctx, dev) // unplugged
		if rep, prev := e.plugRepeated(ctx, dev, "ccd6a0a9-2"); rep || prev != "" {
			t.Fatalf("plug-in -2 100 ms after an unplug: repeat %v, supersedes %q", rep, prev)
		}
		time.Sleep(700 * time.Millisecond) // no unplug
		if rep, prev := e.plugRepeated(ctx, dev, "ccd6a0a9-3"); rep || prev != "ccd6a0a9-2" {
			t.Fatalf("plug-in -3 0.7 s after -2, no unplug: repeat %v, supersedes %q; want a new plug-in superseding -2", rep, prev)
		}
		if rep, _ := e.plugRepeated(ctx, dev, "ccd6a0a9-3"); !rep {
			t.Fatal("-3 reported again counted again")
		}
	}
	check(&engine{st: newMemState()}, "t-tp-replug-mem")
	check(&engine{st: testValkey(t)}, "t-tp-replug-vk")
}
