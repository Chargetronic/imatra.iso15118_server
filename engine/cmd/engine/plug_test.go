// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"testing"
)

// One plug-in reported three times counts once; an unplug ends the window.
func TestPlugCountedOnce(t *testing.T) {
	ctx := context.Background()
	check := func(e *engine, dev string) {
		if e.plugRepeated(ctx, dev) {
			t.Fatal("first plugged taken for a repeat")
		}
		if !e.plugRepeated(ctx, dev) || !e.plugRepeated(ctx, dev) {
			t.Fatal("repeated plugged counted again")
		}
		e.plugForget(ctx, dev)
		if e.plugRepeated(ctx, dev) {
			t.Fatal("a plug-in after an unplug taken for a repeat")
		}
		e.plugForget(ctx, dev)
	}
	check(&engine{st: newMemState()}, "t-tp-plug-mem") // tests without Valkey: in memory
	v := testValkey(t)                                 // the engines: shared in Valkey
	check(&engine{st: v}, "t-tp-plug-vk")
}
