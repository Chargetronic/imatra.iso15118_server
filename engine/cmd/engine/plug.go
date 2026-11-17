// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// A plug-in is counted once (11.11.2026). The device software republished
// its CP state with event "plugged" whenever SLAC changed state at the start
// of a plug-in, so one plug-in became three session.started (and three
// summaries, two of them ended "no unplug seen"). The device now sends the
// event once; the engine also ignores a "plugged" that repeats without an
// "unplugged" in between within PLUG_REPEAT_S (10 s): an EV cannot be
// unplugged and plugged in again without the CP passing through A. A plug-in
// after a lost "unplugged" (offline device) still counts once the window is
// over. With Valkey the window is shared by the instances (a device's
// reports may reach any of them).

import (
	"context"
	"sync"
	"time"
)

var (
	plugMu   sync.Mutex
	plugSeen = map[string]time.Time{}
)

// plugRepeated: true if this device reported "plugged" within the window
// and no "unplugged" since; otherwise the window starts now.
func (e *engine) plugRepeated(ctx context.Context, device string) bool {
	win := time.Duration(envF("PLUG_REPEAT_S", 10) * float64(time.Second))
	if vk := e.vk(); vk != nil {
		ok, err := vk.SetNX(ctx, "plugwin:"+device, 1, win).Result()
		if err == nil {
			return !ok
		}
	}
	plugMu.Lock()
	defer plugMu.Unlock()
	now := time.Now()
	if t, ok := plugSeen[device]; ok && now.Sub(t) < win {
		return true
	}
	plugSeen[device] = now
	return false
}

// plugForget: an unplug ends the window (a quick re-plug is a new plug-in).
func (e *engine) plugForget(ctx context.Context, device string) {
	if vk := e.vk(); vk != nil {
		vk.Del(ctx, "plugwin:"+device)
	}
	plugMu.Lock()
	delete(plugSeen, device)
	plugMu.Unlock()
}
