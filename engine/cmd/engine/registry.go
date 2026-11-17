// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Device registry of the service layer (13.11.2026, block D).
//
// The layer publishes into this deployment's broker, retained, QoS 1, with its
// own certificate (CN "layer", ACL: write registry/# only):
//
//	registry/dev/<device_id>  {"v":1,"device_id","state":"bound|unbound|revoked",
//	                           "type","lat","lon","binding_ver","at","tz"}
//	registry/digest           {"v":1,"bound":N,"max_binding_ver":M,"at"}   once a day
//
// The operator hears only of facts: until "unbound" the device is this
// operator's, after it not. The defaults applied here (an operator may change
// them, the server is open; guarantees come from the layer and the firmware):
//
//	bound    the device's row (it may not have connected yet), type, coarse
//	         location (also into the activation pool); device.bound
//	unbound  out of the activation pool, its limits cancelled; a running
//	         session goes on to its end; device.unbound
//	revoked  served no more at once (the local block's barrier); device.revoked
//
// Only the leader reads it, with an ordinary subscription: a shared one
// ($share) gets no retained messages. A new leader subscribes and gets the
// whole registry again — that is the full reconciliation; a message is
// applied only if its binding_ver is greater than the stored one, so repeats,
// late messages and re-reads change nothing. A device with a valid
// certificate and no registry entry is admitted (the certificate decides);
// engine_registry_unknown_devices counts them.
//
// The daily digest: when the count of bound devices or the greatest
// binding_ver differs from what is stored, the leader reads the registry
// again and writes system.registry_mismatch.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

const (
	registryTopic = "registry/dev/+"
	digestTopic   = "registry/digest"
)

var (
	mRegistryApplied = reg.CounterVec("engine_registry_applied_total",
		"Registry messages applied, by state (bound, unbound, revoked).", "state")
	mRegistryIgnored = reg.Counter("engine_registry_ignored_total",
		"Registry messages not applied: repeats, late or already seen (binding_ver not greater), malformed.")
	mRegistryMismatch = reg.Counter("engine_registry_mismatch_total",
		"Daily digests of the layer that did not match the stored registry (re-read).")
)

type registry struct {
	mu         sync.Mutex
	subscribed bool
	unknown    float64 // devices without a registry entry (last count)
}

// registryLoop: the leader holds the subscription; a new leader takes it over.
func (e *engine) registryLoop() {
	reg.Gauge("engine_registry_unknown_devices",
		"devices admitted by their certificate with no registry entry of the service layer.", func() float64 {
			e.reg.mu.Lock()
			defer e.reg.mu.Unlock()
			return e.reg.unknown
		})
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	n := 0
	for range t.C {
		e.registrySync(false)
		if n++; n%30 == 1 { // a minute; every instance (one cheap query)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, _, u, err := e.db.RegistryStats(ctx); err == nil {
				e.reg.mu.Lock()
				e.reg.unknown = float64(u)
				e.reg.mu.Unlock()
			} else {
				e.log.Warn("registry stats", "err", err)
			}
			cancel()
		}
	}
}

// registrySync subscribes when this instance is the leader, unsubscribes when
// it is not; force: subscribe again (after a reconnect, or to re-read).
func (e *engine) registrySync(force bool) {
	if e.client == nil || !e.client.IsConnectionOpen() {
		return
	}
	lead := e.coord.isLeader()
	e.reg.mu.Lock()
	defer e.reg.mu.Unlock()
	switch {
	case lead && (force || !e.reg.subscribed):
		if e.reg.subscribed {
			e.client.Unsubscribe(registryTopic, digestTopic).WaitTimeout(5 * time.Second)
		}
		e.client.Subscribe(registryTopic, 1, e.onRegistry)
		e.client.Subscribe(digestTopic, 1, e.onDigest)
		e.reg.subscribed = true
		e.log.Info("registry: reading (leader)")
	case !lead && e.reg.subscribed:
		e.client.Unsubscribe(registryTopic, digestTopic)
		e.reg.subscribed = false
		e.log.Info("registry: not the leader any more")
	}
}

func (e *engine) onRegistry(_ mqtt.Client, m mqtt.Message) {
	var r store.RegistryEntry
	dev := strings.TrimPrefix(m.Topic(), "registry/dev/")
	if json.Unmarshal(m.Payload(), &r) != nil || r.Device != dev || r.BindingVer <= 0 ||
		(r.State != "bound" && r.State != "unbound" && r.State != "revoked") || !validDeviceID(dev) {
		mRegistryIgnored.Inc()
		return
	}
	// On the device's worker: in order with its other messages.
	e.w.do(dev, func() { e.applyRegistry(r) })
}

func (e *engine) applyRegistry(r store.RegistryEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	at, err := time.Parse(time.RFC3339, r.At)
	if err != nil {
		at = time.Now().UTC()
	}
	ok, prev, err := e.db.ApplyRegistry(ctx, r, at)
	if err != nil {
		e.log.Error("registry", "device", r.Device, "err", err)
		return
	}
	if !ok {
		mRegistryIgnored.Inc()
		return
	}
	mRegistryApplied.With(r.State).Inc()
	data := map[string]any{"binding_ver": r.BindingVer}
	if r.Type != "" {
		data["type"] = r.Type
	}
	if r.TZ != "" {
		data["tz"] = r.TZ
	}
	if len(r.Rules) > 0 {
		data["rules"] = r.Rules
		// the owner of the device reads its station limits again (rules.go)
		if err := e.db.NotifyConfig(ctx, r.Device); err != nil {
			e.log.Warn("registry: notify rules", "device", r.Device, "err", err)
		}
	}
	if prev != "" {
		data["previous"] = prev
	}
	typ := events.DeviceBound
	switch r.State {
	case "bound":
		if r.Lat != nil && r.Lon != nil {
			data["lat"], data["lon"] = round3(*r.Lat), round3(*r.Lon)
		}
		if prev == "revoked" {
			if err := e.db.UnrevokeDevice(ctx, r.Device); err != nil {
				e.log.Error("registry unrevoke", "device", r.Device, "err", err)
			}
		}
	case "unbound":
		typ = events.DeviceUnbound
		if n, err := e.db.CancelLimits(ctx, r.Device); err != nil {
			e.log.Error("registry: cancel limits", "device", r.Device, "err", err)
		} else if n > 0 {
			data["limits_cancelled"] = n
		}
	case "revoked":
		typ = events.DeviceRevoked
		if err := e.db.RevokeDevice(ctx, r.Device); err != nil {
			e.log.Error("registry revoke", "device", r.Device, "err", err)
		}
	}
	// The feed event also updates the pool record (luaHot: out of / back in the pool).
	e.emit(ctx, events.Event{Type: typ, Device: r.Device, Time: at.UTC(), Data: data})
	e.log.Info("registry", "device", r.Device, "state", r.State, "binding_ver", r.BindingVer, "previous", prev)
}

func (e *engine) onDigest(_ mqtt.Client, m mqtt.Message) {
	var d struct {
		Bound  int64  `json:"bound"`
		MaxVer int64  `json:"max_binding_ver"`
		At     string `json:"at"`
	}
	if json.Unmarshal(m.Payload(), &d) != nil {
		return
	}
	go func() {
		// Messages published just before the digest may still be on the
		// workers: compare a little later.
		time.Sleep(5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bound, maxVer, _, err := e.db.RegistryStats(ctx)
		if err != nil || (bound == d.Bound && maxVer == d.MaxVer) {
			return
		}
		mRegistryMismatch.Inc()
		e.log.Warn("registry: digest differs, reading again", "layer_bound", d.Bound, "bound", bound,
			"layer_max_ver", d.MaxVer, "max_ver", maxVer)
		e.emit(ctx, events.Event{Type: events.RegistryMismatch, Time: time.Now().UTC(), Data: map[string]any{
			"layer_bound": d.Bound, "bound": bound, "layer_max_binding_ver": d.MaxVer, "max_binding_ver": maxVer}})
		e.registrySync(true)
	}()
}

func round3(x float64) float64 { return float64(int64(x*1000+sign(x)*0.5)) / 1000 }

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

// validDeviceID: the characters a device id (certificate CN) may have.
func validDeviceID(s string) bool {
	if len(s) < 3 || len(s) > 63 || s[0] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return s != "server" && s != "layer"
}
