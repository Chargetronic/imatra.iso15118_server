// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/evexi"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
)

// Profile of the EV behind a virtual device.
type Profile string

const (
	ProfileISO Profile = "iso" // ISO 15118-2 AC EIM: digital session, Charging Loop cache
	ProfilePWM Profile = "pwm" // no HLC: basic charging, the limit goes through power/set
)

// device is one virtual device with its EV. Protocol as in
// the device protocol (internal/tunnel); EXI from internal/evexi.
type device struct {
	id      string
	idx     int
	profile Profile
	o       *options
	m       *metrics
	tlsConf *tls.Config

	client mqtt.Client

	mu        sync.Mutex
	cfg       tunnel.Config
	conn      string // current V2G connection
	seq       uint32
	pending   map[uint32]chan *tunnel.Down
	pairReq   []byte // Charging Loop cache: request bytes -> response
	pairRes   []byte
	limitA    float64 // current limit seen by the EV (cache answer or power/set)
	inSession bool
	sessionID []byte // V2G SessionID from SessionSetupRes
	tuple     uint8  // SAScheduleTupleID from ChargeParameterDiscoveryRes

	// delivery of operator commands: set by the activation, read on arrival
	cmdAt     time.Time
	onDeliver func(d *device, at time.Time)

	// ADR-003: ids instead of the device clock (as device software 0.3.2+)
	boot    string // this start of the device
	plugN   int
	plug    string // "<boot>-<n>" while an EV is plugged in
	plugAt  time.Time
	teleSeq int64 // telemetry number within the boot

	// reconnect measurement
	droppedAt   time.Time
	onReconnect func(d *device, at time.Time)
	onPairBack  func(d *device, at time.Time)
}

func newDevice(idx int, o *options, m *metrics, p Profile, tlsConf *tls.Config) *device {
	return &device{
		id: fmt.Sprintf("%s%05d", o.prefix, idx), idx: idx, profile: p, o: o, m: m, tlsConf: tlsConf,
		pending: map[uint32]chan *tunnel.Down{}, limitA: o.evMaxA, boot: fmt.Sprintf("%08x", rand.Uint32()),
		cfg: tunnel.Config{SampleS: 30, HeartbeatS: 60, TelemetryS: 10, CacheEnabled: true},
	}
}

func (d *device) topic(s string) string { return tunnel.Root + "/" + d.id + "/" + s }

// plugTopics: the messages that carry the plug-in id (as a device does).
var plugTopics = map[string]bool{"cp": true, "telemetry/energy": true, "cmd/ack": true, "v2g/up": true}

func (d *device) publish(suffix string, v any, qos byte, retain bool) {
	b, _ := json.Marshal(v)
	d.mu.Lock()
	plug := d.plug
	d.mu.Unlock()
	if (plugTopics[suffix] && plug != "" && !d.o.legacy) || d.o.clockSkew != 0 {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if _, has := m["plug"]; !has && plugTopics[suffix] && plug != "" && !d.o.legacy {
				m["plug"] = plug
			}
			if t, ok := m["t"].(float64); ok && t > 0 && d.o.clockSkew != 0 {
				m["t"] = int64(t) + d.o.clockSkew.Microseconds()
			}
			b, _ = json.Marshal(m)
		}
	}
	d.client.Publish(d.topic(suffix), qos, retain, b)
}

// plugIn: a new plug-in id; unplugged: its duration on the monotonic clock.
func (d *device) plugIn() {
	d.mu.Lock()
	d.plugN++
	d.plug, d.plugAt = fmt.Sprintf("%s-%d", d.boot, d.plugN), time.Now()
	d.mu.Unlock()
}

// unplug publishes the unplug report (late with -late-unplug: the device
// was offline), with the plug-in's own duration, then forgets the id.
func (d *device) unplug(st tunnel.CPState) {
	d.mu.Lock()
	ms := time.Since(d.plugAt).Milliseconds()
	d.mu.Unlock()
	if !d.o.legacy {
		st.PluggedMs = &ms
	}
	if d.o.lateUnplug > 0 {
		time.Sleep(d.o.lateUnplug)
	}
	st.T = time.Now().UnixMicro()
	d.publish("cp", st, 1, true)
	d.mu.Lock()
	d.plug = ""
	d.mu.Unlock()
}

// connect opens the MQTT connection; returns when the broker accepted it.
func (d *device) connect() error {
	opts := mqtt.NewClientOptions().
		AddBroker(d.o.broker).
		SetClientID(d.id+"-fleet").
		SetCleanSession(true).
		SetKeepAlive(time.Duration(d.o.keepaliveS)*time.Second).
		SetAutoReconnect(false).
		SetConnectTimeout(30*time.Second).
		SetWill(d.topic("status"), `{"online":false}`, 1, true).
		SetOrderMatters(false)
	if d.tlsConf != nil {
		opts.SetTLSConfig(d.tlsConf)
	}
	if d.tlsConf == nil || len(d.tlsConf.Certificates) == 0 {
		opts.SetUsername(d.id).SetPassword(d.o.password) // a broker with password accounts (not the deployment: certificates only)
	}
	opts.SetOnConnectHandler(d.onConnect)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) { d.m.inc("mqtt.connection_lost") })
	d.client = mqtt.NewClient(opts)
	t0 := time.Now()
	tok := d.client.Connect()
	if !tok.WaitTimeout(35*time.Second) || tok.Error() != nil {
		d.m.inc("mqtt.connect_failed")
		if tok.Error() != nil {
			return tok.Error()
		}
		return errors.New("connect timeout")
	}
	d.m.observe("mqtt.connect", time.Since(t0))
	return nil
}

func (d *device) onConnect(c mqtt.Client) {
	c.SubscribeMultiple(map[string]byte{
		d.topic("v2g/down"): 0, d.topic("cl/push"): 1, d.topic("ctrl"): 1, d.topic("config"): 1,
		d.topic("cp/set"): 1, d.topic("power/set"): 1,
	}, d.onMessage)
	d.mu.Lock()
	dropped, cb := d.droppedAt, d.onReconnect
	d.mu.Unlock()
	d.publishStatus("connect")
	if !dropped.IsZero() && cb != nil {
		cb(d, time.Now())
	}
}

func (d *device) publishStatus(event string) {
	d.mu.Lock()
	st := map[string]any{"online": true, "role": "fleetgen", "sw": "fleetgen", "platform": "virtual",
		"ev_connected": d.inSession, "loop_active": d.inSession && d.pairReq != nil,
		"config_ver": d.cfg.Ver, "config_rev": d.cfg.Rev, "t": time.Now().UnixMicro()}
	d.mu.Unlock()
	if event != "" {
		st["event"] = event
	}
	d.publish("status", st, 1, true)
}

func (d *device) onMessage(_ mqtt.Client, msg mqtt.Message) {
	now := time.Now()
	t := msg.Topic()
	switch {
	case hasSuffix(t, "/v2g/down"):
		var dn tunnel.Down
		if json.Unmarshal(msg.Payload(), &dn) != nil {
			return
		}
		d.mu.Lock()
		acked := dn.CL != nil && dn.Conn == d.conn
		if acked {
			d.installLocked(dn.CL)
		}
		ch := d.pending[dn.Seq]
		delete(d.pending, dn.Seq)
		d.mu.Unlock()
		if acked {
			d.ack("pair", dn.CL.Ver, dn.CL.CommandID, dn.Conn, "installed")
		}
		if ch != nil {
			ch <- &dn
		}
	case hasSuffix(t, "/cl/push"):
		var p tunnel.Push
		if json.Unmarshal(msg.Payload(), &p) != nil || p.CL == nil {
			return
		}
		d.m.inc("rx.cl_push")
		d.mu.Lock()
		ok := p.Conn == d.conn
		if ok {
			d.installLocked(p.CL)
		}
		dropped, pb := d.droppedAt, d.onPairBack
		d.mu.Unlock()
		if ok {
			d.ack("pair", p.CL.Ver, p.CL.CommandID, p.Conn, "installed")
			d.delivered(now)
			if !dropped.IsZero() && pb != nil {
				pb(d, now)
			}
		}
	case hasSuffix(t, "/power/set"):
		var p tunnel.PowerSet
		if json.Unmarshal(msg.Payload(), &p) != nil {
			return
		}
		d.m.inc("rx.power_set")
		d.mu.Lock()
		pwmSide := d.profile == ProfilePWM && d.inSession
		d.mu.Unlock()
		switch {
		case p.LimitA <= 0:
			d.ack("power", p.Ver, p.CommandID, "", "cleared")
		case pwmSide:
			d.ack("power", p.Ver, p.CommandID, "", "applied")
		default: // ISO: held by the device, the pair carries the limit
			d.ack("power", p.Ver, p.CommandID, "", "held")
		}
		if d.profile == ProfilePWM && p.CommandID != 0 && p.LimitA > 0 {
			d.mu.Lock()
			d.limitA = p.LimitA
			in := d.inSession
			d.mu.Unlock()
			if in {
				// the mask reached the CP: the engine confirms the command from this report
				duty := p.LimitA / 0.6
				la := p.LimitA
				d.publish("cp", tunnel.CPState{Mode: "basic", Out: "mask", Duty: duty, LimitA: &la, EV: "C",
					Station: map[string]any{"duty": 53.3}, T: now.UnixMicro()}, 1, true)
			}
			d.delivered(now)
		}
	case hasSuffix(t, "/ctrl"):
		d.m.inc("rx.ctrl")
	case hasSuffix(t, "/config"):
		var c tunnel.Config
		if json.Unmarshal(msg.Payload(), &c) != nil {
			return
		}
		d.m.inc("rx.config")
		d.mu.Lock()
		if c.Ver > d.cfg.Ver {
			if d.o.sampleS > 0 {
				c.SampleS = d.o.sampleS
			}
			if d.o.telemetryS > 0 {
				c.TelemetryS = d.o.telemetryS
			}
			d.cfg = c
		}
		d.mu.Unlock()
	case hasSuffix(t, "/cp/set"):
		d.m.inc("rx.cp_set")
	}
}

// ack: cmd/ack as a device sends it.
func (d *device) ack(kind string, ver, commandID int64, conn, result string) {
	d.m.inc("tx.ack." + kind)
	d.publish("cmd/ack", tunnel.Ack{V: tunnel.Version, Kind: kind, Ver: ver, CommandID: commandID, Conn: conn,
		Result: result, T: time.Now().UnixMicro()}, 1, false)
}

func (d *device) installLocked(cl *tunnel.CachePair) {
	if len(cl.Req) == 0 || len(cl.Res) == 0 {
		return
	}
	d.pairReq, d.pairRes = cl.Req, cl.Res
	if r, err := evexi.DecodeRes(cl.Res); err == nil && r.HasMaxCurrent {
		d.limitA = r.MaxCurrentA
	}
}

func (d *device) delivered(at time.Time) {
	d.mu.Lock()
	cmd, cb := d.cmdAt, d.onDeliver
	if !cmd.IsZero() {
		d.cmdAt = time.Time{}
	}
	d.mu.Unlock()
	if !cmd.IsZero() && cb != nil {
		cb(d, at)
	}
}

// request sends one EV request to the server and waits for the answer.
func (d *device) request(ctx context.Context, kind evexi.Kind, exi []byte) (*tunnel.Down, error) {
	ch := make(chan *tunnel.Down, 1)
	d.mu.Lock()
	d.seq++
	seq, conn := d.seq, d.conn
	d.pending[seq] = ch
	d.mu.Unlock()
	t0 := time.Now()
	d.publish("v2g/up", tunnel.Up{V: 1, Conn: conn, Seq: seq, PT: 0x8001, EXI: exi, WireBytes: len(exi) + 8,
		TEVRx: t0.UnixMicro(), TUpTx: t0.UnixMicro()}, 0, false)
	select {
	case dn := <-ch:
		d.m.observe("v2g."+kind.String(), time.Since(t0))
		d.m.observe("v2g.all", time.Since(t0))
		return dn, nil
	case <-time.After(d.timeout(kind)):
		d.mu.Lock()
		delete(d.pending, seq)
		d.mu.Unlock()
		d.m.inc("v2g.timeout." + kind.String())
		return nil, fmt.Errorf("%v: no answer", kind)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// timeout: how long the EV waits for the answer before it ends the session —
// V2G_EVCC_Msg_Timeout of ISO 15118-2, table 109 (PowerDelivery 5 s, the
// other messages used here 2 s). -timeout > 0 overrides it for every message.
func (d *device) timeout(kind evexi.Kind) time.Duration {
	if d.o.timeoutS > 0 {
		return time.Duration(d.o.timeoutS * float64(time.Second))
	}
	if kind == evexi.PowerDelivery {
		return 5 * time.Second
	}
	return 2 * time.Second
}

func (d *device) step(ctx context.Context, sid []byte, r *evexi.Req) (*evexi.Res, *tunnel.Down, error) {
	r.SessionID = sid
	exi, err := evexi.EncodeReq(r)
	if err != nil {
		return nil, nil, err
	}
	dn, err := d.request(ctx, r.Kind, exi)
	if err != nil {
		return nil, nil, err
	}
	if len(dn.EXI) == 0 {
		return nil, dn, fmt.Errorf("%v: empty answer (close=%v)", r.Kind, dn.Close)
	}
	res, err := evexi.DecodeRes(dn.EXI)
	if err != nil {
		return nil, dn, err
	}
	if res.ResponseCode > 1 {
		d.m.inc("v2g.failed_code." + r.Kind.String())
		return res, dn, fmt.Errorf("%v: response code %d", r.Kind, res.ResponseCode)
	}
	return res, dn, nil
}

// session runs one plug-in: connection setup, charging for chargeFor, end.
func (d *device) session(ctx context.Context, chargeFor time.Duration) {
	now := time.Now()
	d.mu.Lock()
	d.inSession = true
	d.conn = fmt.Sprintf("%016x", rand.Uint64())[:12]
	d.seq = 0
	d.pairReq, d.pairRes = nil, nil
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inSession = false
		d.mu.Unlock()
	}()
	d.m.inc("session.started")

	d.plugIn()
	if d.profile == ProfilePWM {
		d.publish("cp", tunnel.CPState{Mode: "transit", Out: "transit", EV: "C", Event: "plugged",
			Station: map[string]any{"duty": 53.3}, T: now.UnixMicro()}, 1, true)
		d.charge(ctx, chargeFor, false)
		d.unplug(tunnel.CPState{Mode: "transit", Out: "transit", EV: "A", Event: "unplugged"})
		d.m.inc("session.ended")
		return
	}

	d.publish("cp", tunnel.CPState{Mode: "digital", Out: "mask", Duty: 5, EV: "B", Event: "plugged",
		Station: map[string]any{"duty": 53.3}, Slac: "MATCHED", T: now.UnixMicro()}, 1, true)
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	d.publish("v2g/up", tunnel.Up{V: 1, Conn: conn, Event: "open", TEVRx: now.UnixMicro(), TUpTx: now.UnixMicro()}, 0, false)

	t0 := time.Now()
	reason := "EV unplugged"
	ok := d.isoSetup(ctx)
	if ok {
		d.m.observe("session.setup_total", time.Since(t0))
		d.charge(ctx, chargeFor, true)
		sid := d.sessionID
		if _, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.PowerDelivery, ChargeProgress: 1, SATupleID: d.tuple}); err == nil {
			_, _, _ = d.step(ctx, sid, &evexi.Req{Kind: evexi.SessionStop})
		}
		reason = "EV closed the connection"
	} else {
		d.m.inc("session.setup_failed")
	}
	d.mu.Lock()
	seq := d.seq
	d.mu.Unlock()
	d.publish("v2g/up", tunnel.Up{V: 1, Conn: conn, Seq: seq + 1, Event: "closed", Reason: reason,
		TEVRx: time.Now().UnixMicro(), TUpTx: time.Now().UnixMicro()}, 0, false)
	d.unplug(tunnel.CPState{Mode: "digital", Out: "mask", Duty: 5, EV: "A", Event: "unplugged"})
	d.m.inc("session.ended")
}

func (d *device) isoSetup(ctx context.Context) bool {
	ah, _ := evexi.EncodeAppHandReq()
	if _, err := d.request(ctx, 0, ah); err != nil {
		return false
	}
	evcc := [6]byte{0x02, 0xfe, byte(d.idx >> 24), byte(d.idx >> 16), byte(d.idx >> 8), byte(d.idx)}
	r, _, err := d.step(ctx, nil, &evexi.Req{Kind: evexi.SessionSetup, EVCCID: evcc})
	if err != nil {
		return false
	}
	d.sessionID = r.SessionID
	sid := r.SessionID
	if _, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.ServiceDiscovery}); err != nil {
		return false
	}
	if _, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.PaymentServiceSelection}); err != nil {
		return false
	}
	for i := 0; i < 30; i++ {
		r, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.Authorization})
		if err != nil {
			return false
		}
		if !r.Ongoing {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var cpd *evexi.Res
	for i := 0; i < 30; i++ {
		r, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.ChargeParameterDiscovery,
			DepartureS: uint32(6*3600 + rand.Intn(6*3600)), EAmountWh: int32(5000 + rand.Intn(40000)),
			MaxCurrentA: int32(d.o.evMaxA)})
		if err != nil {
			return false
		}
		cpd = r
		if !r.Ongoing {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.tuple = uint8(cpd.SATupleID)
	if _, _, err := d.step(ctx, sid, &evexi.Req{Kind: evexi.PowerDelivery, ChargeProgress: 0, SATupleID: d.tuple}); err != nil {
		return false
	}
	d.publish("cp", tunnel.CPState{Mode: "digital", Out: "mask", Duty: 5, EV: "C",
		Station: map[string]any{"duty": 53.3}, T: time.Now().UnixMicro()}, 1, true)
	return true
}

// charge runs the Charging Loop (iso) and the metering until done.
func (d *device) charge(ctx context.Context, dur time.Duration, iso bool) {
	end := time.Now().Add(dur)
	loop := time.NewTicker(time.Duration(d.o.loopMs) * time.Millisecond)
	defer loop.Stop()
	var lastSample, lastTele time.Time
	var eWh float64
	var sentA float64 // current of the last report
	csReq, _ := evexi.EncodeReq(&evexi.Req{Kind: evexi.ChargingStatus, SessionID: d.sessionID})
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-loop.C:
			if now.After(end) {
				return
			}
			d.mu.Lock()
			cfg, limit := d.cfg, d.limitA
			hit := iso && cfg.CacheEnabled && d.pairReq != nil && bytes.Equal(d.pairReq, csReq)
			conn := d.conn
			d.mu.Unlock()
			if iso {
				if hit {
					d.m.inc("loop.cache_hit")
					if cfg.SampleS == 0 || now.Sub(lastSample) >= time.Duration(cfg.SampleS)*time.Second {
						lastSample = now
						d.mu.Lock()
						d.seq++
						seq := d.seq
						d.mu.Unlock()
						d.publish("v2g/up", tunnel.Up{V: 1, Conn: conn, Seq: seq, PT: 0x8001, EXI: csReq, Sampled: true,
							TEVRx: now.UnixMicro(), TUpTx: now.UnixMicro()}, 0, false)
						d.m.inc("tx.sampled")
					}
				} else {
					d.m.inc("loop.to_server")
					if _, _, err := d.step(ctx, d.sessionID, &evexi.Req{Kind: evexi.ChargingStatus}); err != nil {
						d.m.inc("loop.error")
					}
				}
			}
			// like a device: every telemetry_s, and at once when a
			// phase current changes by 1 A or more (a limit took effect)
			changed := !lastTele.IsZero() && math.Abs(limit-sentA) >= 1
			if cfg.TelemetryS > 0 && (changed || now.Sub(lastTele) >= time.Duration(cfg.TelemetryS)*time.Second) {
				if !lastTele.IsZero() {
					eWh += sentA * 3 * 230 * now.Sub(lastTele).Hours()
				}
				lastTele, sentA = now, limit
				d.teleSeq++
				tele := map[string]any{"v": 1, "seq": d.teleSeq, "t": now.UnixMicro(),
					"i_a": []float64{limit, limit, limit}, "p_total_w": limit * 3 * 230, "e_wh": eWh,
					"ev": "C", "limit_a": limit}
				if !d.o.legacy {
					tele["boot"] = d.boot
				}
				d.publish("telemetry/energy", tele, 1, false)
				d.m.inc("tx.telemetry")
			}
		}
	}
}

// heartbeat publishes status every heartbeat_s.
func (d *device) heartbeat(ctx context.Context) {
	for {
		d.mu.Lock()
		p := time.Duration(d.cfg.HeartbeatS) * time.Second
		d.mu.Unlock()
		if p <= 0 {
			p = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p):
			if d.client.IsConnected() {
				d.publishStatus("")
				d.m.inc("tx.status")
			}
		}
	}
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }
