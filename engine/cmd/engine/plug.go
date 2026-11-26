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
//
// The plug-in id decides (issue #15, 26.11.2026). A "plugged" within the
// window with another plug-in id than the one that opened the window is not a
// repeat: the device counts a new plug-in, and every report after it carries
// the new id. Taken for a repeat, it left the new id without a
// session.started: its limit.applied and session.ended waited in
// projector_orphans for a plug-in that never appeared, and the earlier one
// was closed "no unplug seen" by the next plug-in (a harness station on
// aarch64, the id changed 0.9 s after the plug-in, no "unplugged" between).
// Such a "plugged" starts a plug-in that supersedes the earlier one
// (session.started data "supersedes"). The engine does not invent an unplug
// for the earlier id: the device reported none.

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	plugMu   sync.Mutex
	plugSeen = map[string]plugMark{}
)

type plugMark struct {
	at   time.Time
	plug string
}

// plugCheckScript: KEYS[1] the device's window, ARGV[1] the plug-in id
// ("" from a device before 0.3.2), ARGV[2] the window in ms. Returns
// {repeat, the id that opened the window}.
var plugCheckScript = redis.NewScript(`
local old = redis.call('GET', KEYS[1])
if old and (old == ARGV[1] or old == '' or ARGV[1] == '') then return {1, old} end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
if old then return {0, old} end
return {0, ''}
`)

func plugKey(device string) string { return "plugwin:" + device }

// plugRepeated: true if this device reported "plugged" for the same plug-in
// (same id, or no ids) within the window and no "unplugged" since; otherwise
// the window starts now. prev: the id of the plug-in that opened the window
// when this one has another id (the earlier plug-in had no unplug).
func (e *engine) plugRepeated(ctx context.Context, device, plug string) (repeat bool, prev string) {
	win := time.Duration(envF("PLUG_REPEAT_S", 10) * float64(time.Second))
	if vk := e.vk(); vk != nil {
		r, err := plugCheckScript.Run(ctx, vk, []string{plugKey(device)}, plug, win.Milliseconds()).Slice()
		if err == nil && len(r) == 2 {
			n, _ := r[0].(int64)
			old, _ := r[1].(string)
			if n == 1 {
				return true, ""
			}
			return false, old
		}
	}
	plugMu.Lock()
	defer plugMu.Unlock()
	now := time.Now()
	m, ok := plugSeen[device]
	if ok && now.Sub(m.at) < win && (m.plug == plug || m.plug == "" || plug == "") {
		return true, ""
	}
	plugSeen[device] = plugMark{at: now, plug: plug}
	if ok && now.Sub(m.at) < win {
		return false, m.plug
	}
	return false, ""
}

// plugForget: an unplug ends the window (a quick re-plug is a new plug-in).
func (e *engine) plugForget(ctx context.Context, device string) {
	if vk := e.vk(); vk != nil {
		vk.Del(ctx, plugKey(device))
	}
	plugMu.Lock()
	delete(plugSeen, device)
	plugMu.Unlock()
}
