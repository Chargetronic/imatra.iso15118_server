// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Final SoC read (08.11.2026). A DIN-only EV (no ISO 15118-2) gives its SoC
// at plug-in in a DIN 70121 handshake and then charges in basic mode on the
// station PWM. When it stops charging by itself (CP C -> B on the PWM side,
// held for soc_final_wait_s), the engine puts 5 % on the line once more: the
// EV starts another DIN handshake, the engine reads the final SoC and goes
// back to the PWM side. Two readings and the energy between them give the
// battery capacity.
//
// Once per plug-in. An EV that does not answer in socFinalTimeout goes back
// to the PWM side (soc.final result "no_answer"); unplugging ends it too.
// Events: soc.final.requested, soc.read (reading "final"), soc.final.
//
// The waits are timers in this process: after an engine restart a pending one
// is lost (the EV stays on 5 % until the device's hlc_wait or unplug).

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
)

const (
	socFinalTimeout = 60 * time.Second // 5 % on the line, no SoC: back to the PWM side
	socFinalMinWh   = 50.0             // the EV must have charged since the first reading
)

type finalReads struct {
	mu      sync.Mutex
	lastEV  map[string]string      // device -> last EV CP state reported
	wait    map[string]*time.Timer // device -> C -> B seen, waiting
	pending map[string]*pendingRead
}

type pendingRead struct {
	at      time.Time
	energy  float64 // Wh since the first reading, when 5 % went on
	timeout *time.Timer
}

func newFinalReads() finalReads {
	return finalReads{lastEV: map[string]string{}, wait: map[string]*time.Timer{}, pending: map[string]*pendingRead{}}
}

func (e *engine) finalConfig(ctx context.Context, device string) (tunnel.Config, bool) {
	conf := e.dev
	if _, err := e.db.DeviceSettings(ctx, device, &conf); err != nil {
		return conf, false
	}
	return conf, conf.DigitalMode && conf.SoCFinal
}

// watchStop: every CP report. C/D -> B on the PWM side starts the wait; any
// other change cancels it.
func (e *engine) watchStop(device string, st tunnel.CPState) {
	f := &e.final
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.lastEV[device]
	f.lastEV[device] = st.EV
	if prev == st.EV {
		return
	}
	if t := f.wait[device]; t != nil {
		t.Stop()
		delete(f.wait, device)
	}
	pwmSide := st.Mode == "transit" || st.Mode == "basic"
	if (prev == "C" || prev == "D") && st.EV == "B" && pwmSide && f.pending[device] == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conf, on := e.finalConfig(ctx, device)
		cancel()
		if !on {
			return
		}
		wait := time.Duration(max(conf.SoCFinalWaitS, 1)) * time.Second
		f.wait[device] = time.AfterFunc(wait, func() { e.finalStart(device, wait) })
	}
}

// finalStart: the EV stayed in B. Read again if this plug-in began with a SoC
// read, has no final read yet and the EV took energy since.
func (e *engine) finalStart(device string, waited time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := &e.final
	f.mu.Lock()
	if f.lastEV[device] != "B" || f.pending[device] != nil {
		f.mu.Unlock()
		return
	}
	delete(f.wait, device)
	f.mu.Unlock()

	since, err := e.db.PluggedSince(ctx, device)
	if err != nil || since.IsZero() {
		return
	}
	n, first, err := e.db.EventsSince(ctx, device, "soc.read", since)
	if err != nil || n == 0 {
		return // no SoC read at plug-in: an ISO 15118-2 or a PWM-only EV
	}
	if done, _, err := e.db.EventsSince(ctx, device, "soc.final.requested", since); err != nil || done > 0 {
		return
	}
	wh, err := e.db.Energy(ctx, device, first)
	if err != nil || wh < socFinalMinWh {
		e.log.Info("final SoC read skipped: too little energy since the first reading", "device", device, "energy_wh", wh)
		return
	}
	p := &pendingRead{at: time.Now(), energy: wh}
	p.timeout = time.AfterFunc(socFinalTimeout, func() { e.finalTimeout(device, p) })
	f.mu.Lock()
	f.pending[device] = p
	f.mu.Unlock()

	b, _ := json.Marshal(tunnel.CPSet{V: tunnel.Version, Mode: "digital"})
	e.client.Publish(tunnel.CPSetTopic(device), 1, false, b)
	e.addEvent(ctx, device, "", secc.Event{Type: "soc.final.requested", Data: map[string]any{
		"reason": "the EV stopped charging (C -> B on the PWM side)", "waited_s": waited.Seconds(),
		"energy_since_first_read_wh": round2(wh)}})
	e.addEvent(ctx, device, "", secc.Event{Type: "cp.mode", Data: map[string]any{
		"mode": "digital", "reason": "final SoC read: 5 % for a DIN handshake"}})
	e.log.Info("final SoC read: 5 % to the EV", "device", device, "energy_wh", wh)
}

// onSoCRead marks the reading (start / final) before it is journalled.
func (e *engine) onSoCRead(ctx context.Context, device, sessionID string, ev secc.Event) {
	data, _ := ev.Data.(map[string]any)
	if data == nil {
		return
	}
	f := &e.final
	f.mu.Lock()
	p := f.pending[device]
	delete(f.pending, device)
	f.mu.Unlock()
	if p == nil {
		data["reading"] = "start"
		return
	}
	p.timeout.Stop()
	data["reading"] = "final"
	e.addEvent(ctx, device, sessionID, secc.Event{Type: "soc.final", Data: map[string]any{
		"result": "ok", "soc": data["soc"], "after_s": round2(time.Since(p.at).Seconds()),
		"energy_since_first_read_wh": round2(p.energy)}})
	// afterSoC (the connection closes) puts the EV back on the PWM side
}

func (e *engine) finalTimeout(device string, p *pendingRead) {
	f := &e.final
	f.mu.Lock()
	if f.pending[device] != p {
		f.mu.Unlock()
		return
	}
	delete(f.pending, device)
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(tunnel.CPSet{V: tunnel.Version, Mode: "transit"})
	e.client.Publish(tunnel.CPSetTopic(device), 1, false, b)
	e.addEvent(ctx, device, "", secc.Event{Type: "soc.final", Data: map[string]any{
		"result": "no_answer", "after_s": socFinalTimeout.Seconds(),
		"energy_since_first_read_wh": round2(p.energy)}})
	e.addEvent(ctx, device, "", secc.Event{Type: "cp.mode", Data: map[string]any{
		"mode": "transit", "reason": "final SoC read: no DIN handshake from the EV, back to the PWM side"}})
	e.log.Warn("final SoC read: no answer, PWM side", "device", device)
}

func (e *engine) finalUnplugged(ctx context.Context, device, sid string) {
	f := &e.final
	f.mu.Lock()
	p := f.pending[device]
	delete(f.pending, device)
	if t := f.wait[device]; t != nil {
		t.Stop()
		delete(f.wait, device)
	}
	f.lastEV[device] = "A"
	f.mu.Unlock()
	if p != nil {
		p.timeout.Stop()
		e.addEvent(ctx, device, sid, secc.Event{Type: "soc.final", Data: map[string]any{"result": "unplugged",
			"after_s": round2(time.Since(p.at).Seconds())}})
	}
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
