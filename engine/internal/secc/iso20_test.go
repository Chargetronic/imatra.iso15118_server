// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/evexi"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

var sid20 = []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}

var cfg20 = Config{EVSEID: "FI*CTR*E20TEST", MaxCurrentA: 16, NominalVoltageV: 230, Phases: 3, ISO20: true}

// ev20 drives one ISO 15118-20 AC EIM session against Step, as an EV would.
type ev20 struct {
	t   *testing.T
	st  *State
	cfg Config
	env Env
	sid []byte
	now time.Time
}

func newEV20(t *testing.T, tls string, cfg Config) *ev20 {
	e := &ev20{t: t, cfg: cfg, sid: make([]byte, 8), now: time.Unix(1_800_000_000, 0)}
	e.env = Env{Now: func() time.Time { return e.now }, Random: func(b []byte) { copy(b, sid20) }}
	e.st = NewState(e.now)
	e.st.TLS = tls
	return e
}

func (e *ev20) handshake() *Result {
	ah, err := evexi.EncodeAppHandReq20()
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := Step(e.st, e.cfg, e.env, ah)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *ev20) send(q *exi.Request20) *exi.Response20 {
	e.t.Helper()
	if q.SessionID == nil {
		q.SessionID = e.sid
	}
	q.TimeStamp = uint64(e.now.Unix())
	b, pt, err := exi.EncodeISO20Req(q)
	if err != nil {
		e.t.Fatalf("%v: encode: %v", q.Kind, err)
	}
	r, err := StepPT(e.st, e.cfg, e.env, pt, b)
	if err != nil {
		e.t.Fatalf("%v: step: %v", q.Kind, err)
	}
	if r.PT != q.Kind.PayloadType() {
		e.t.Fatalf("%v: response in payload type 0x%04x", q.Kind, r.PT)
	}
	res, err := exi.DecodeISO20Res(r.PT, r.EXI)
	if err != nil {
		e.t.Fatalf("%v: decode the response: %v", q.Kind, err)
	}
	if res.Kind != q.Kind {
		e.t.Fatalf("%v answered with %v", q.Kind, res.Kind)
	}
	if res.TimeStamp != uint64(e.now.Unix()) {
		e.t.Fatalf("%v: response time stamp %d", q.Kind, res.TimeStamp)
	}
	e.t.Logf("%-30s -> %-30s %-26s %3d B  %s", r.RequestType, r.ResponseType, res.ResponseName, len(r.EXI), e.st.Phase)
	e.now = e.now.Add(200 * time.Millisecond)
	return res
}

func ok(t *testing.T, r *exi.Response20) {
	t.Helper()
	if exi.Failed20(r.ResponseCode) {
		t.Fatalf("%s: %s", r.Type, r.ResponseName)
	}
}

func (e *ev20) loop(mode int, soc int8, watts int16) []byte {
	q := &exi.Request20{Kind: exi.K20ACChargeLoop, SessionID: e.sid, TimeStamp: uint64(e.now.Unix()),
		ControlMode: mode, PresentSoC: &soc, PresentPower: &exi.Rat{Value: watts}}
	if mode == exi.ModeDynamic {
		q.TargetEnergy, q.MaxEnergy, q.MinEnergy = &exi.Rat{Value: 18, Exponent: 3}, &exi.Rat{Value: 40, Exponent: 3}, &exi.Rat{}
		q.MaxPower, q.MinPower = &exi.Rat{Value: 11, Exponent: 3}, &exi.Rat{Value: 1380}
	}
	b, _, err := exi.EncodeISO20Req(q)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// setup runs the session up to the first loop request in the given mode.
func (e *ev20) setup(mode int) {
	t := e.t
	if r := e.handshake(); e.st.Protocol != exi.NamespaceISO20AC || r.PT != exi.PayloadEXI {
		t.Fatalf("handshake chose %q", e.st.Protocol)
	}
	r := e.send(&exi.Request20{Kind: exi.K20SessionSetup, EVCCID: "WVWZZZE1ZMP012345"})
	if r.ResponseCode != exi.RC20OKNewSession || !bytes.Equal(r.SessionID, sid20) || r.EVSEID != e.cfg.EVSEID {
		t.Fatalf("SessionSetupRes %+v", r)
	}
	e.sid = r.SessionID
	ok(t, e.send(&exi.Request20{Kind: exi.K20AuthorizationSetup}))
	r = e.send(&exi.Request20{Kind: exi.K20Authorization, SelectedAuthorization: ptrTo(0)})
	ok(t, r)
	if r.EVSEProcessing != 0 {
		t.Fatal("authorization not finished")
	}
	r = e.send(&exi.Request20{Kind: exi.K20ServiceDiscovery})
	ok(t, r)
	if len(r.Services) != 1 || r.Services[0] != exi.ServiceAC {
		t.Fatalf("services %v", r.Services)
	}
	r = e.send(&exi.Request20{Kind: exi.K20ServiceDetail, ServiceID: ptrTo(uint16(exi.ServiceAC))})
	ok(t, r)
	set := uint16(0)
	for _, p := range r.ParamSets {
		if p.ControlMode == mode {
			set = p.ID
		}
		if p.Connector != 2 || p.NominalVoltage != 230 {
			t.Fatalf("parameter set %+v", p)
		}
	}
	ok(t, e.send(&exi.Request20{Kind: exi.K20ServiceSelection, SelectedServiceID: ptrTo(uint16(exi.ServiceAC)),
		SelectedParameterSetID: &set}))
	r = e.send(&exi.Request20{Kind: exi.K20ACChargeParamDisc, MaxPower: &exi.Rat{Value: 11, Exponent: 3},
		MinPower: &exi.Rat{Value: 1380}})
	ok(t, r)
	if r.EVSEMaxPower.Float() != 16*230*3 {
		t.Fatalf("EVSE max power %v", r.EVSEMaxPower.Float())
	}
	se := &exi.Request20{Kind: exi.K20ScheduleExchange, MaxSupportingPoints: ptrTo(uint16(24)), ControlMode: mode,
		TargetEnergy: &exi.Rat{Value: 20, Exponent: 3}}
	if mode == exi.ModeDynamic {
		se.DepartureTime, se.MaxEnergy, se.MinEnergy = ptrTo(uint32(8*3600)), &exi.Rat{Value: 40, Exponent: 3}, &exi.Rat{}
	}
	r = e.send(se)
	ok(t, r)
	if r.ControlMode != mode {
		t.Fatalf("schedule exchange in mode %d", r.ControlMode)
	}
	if mode == exi.ModeScheduled && (len(r.Schedule) != 1 || r.Schedule[0].Power.Float() < 11_000) {
		t.Fatalf("schedule %+v", r.Schedule)
	}
	if e.st.EV.EAmountWh == nil || *e.st.EV.EAmountWh != 20_000 {
		t.Fatalf("energy need %v", e.st.EV.EAmountWh)
	}
	pd := &exi.Request20{Kind: exi.K20PowerDelivery, EVProcessing: ptrTo(0), ChargeProgress: ptrTo(exi.Progress20Start)}
	if mode == exi.ModeScheduled {
		pd.ProfileMode, pd.SelectedScheduleTuple = exi.ModeScheduled, ptrTo(uint32(1))
	}
	ok(t, e.send(pd))
	if e.st.Phase != PhaseCharging {
		t.Fatalf("phase %s after PowerDelivery(Start)", e.st.Phase)
	}
}

func ptrTo[T any](v T) *T { return &v }

func (e *ev20) stepLoop(b []byte) *exi.Response20 {
	e.t.Helper()
	r, err := StepPT(e.st, e.cfg, e.env, exi.PayloadISO20AC, b)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := exi.DecodeISO20Res(r.PT, r.EXI)
	if err != nil {
		e.t.Fatal(err)
	}
	ok(e.t, res)
	return res
}

// pairReq: the request bytes the device holds with the pair (the engine
// sends st.CLReq as the pair's Req).
var pairReq []byte

// deviceAnswer answers a loop request from the pair as the device firmware
// does (ADR-005): prefix compare, then the current time into the response.
func deviceAnswer(t *testing.T, p *Pair, req []byte, now time.Time) ([]byte, bool) {
	t.Helper()
	if !exi.PrefixMatch(pairReq, req, p.ReqBits) {
		return nil, false
	}
	out := append([]byte(nil), p.Res...)
	if err := exi.WriteTimeStamp(out, p.ResTS, uint64(now.Unix())); err != nil {
		t.Fatal(err)
	}
	return out, true
}

func TestISO20DynamicSession(t *testing.T) {
	e := newEV20(t, "TLSv1.3 TLS_AES_256_GCM_SHA384", cfg20)
	e.setup(exi.ModeDynamic)

	first := e.loop(exi.ModeDynamic, 40, 0)
	r := e.stepLoop(first)
	if r.ControlMode != exi.ModeDynamic || r.TargetPower == nil || r.TargetPower.Float() != 11_000 {
		t.Fatalf("loop response %+v (target: the EV's 11 kW, below the station's 11.04 kW)", r)
	}
	if e.st.EV.SoC == nil || *e.st.EV.SoC != 40 || e.st.EV.EVTargetEnergyWh == nil {
		t.Fatalf("EV data from the loop: %+v", e.st.EV)
	}
	if e.st.CLReqBits == 0 {
		t.Fatal("loop request not cachable")
	}
	pairReq, _ = hex.DecodeString(e.st.CLReq)

	p, err := CachePairOf(e.st, e.cfg, e.now)
	if err != nil || p == nil {
		t.Fatalf("pair: %v %v", p, err)
	}
	if p.PT != exi.PayloadISO20AC || len(p.Term) != 3 || p.TermPT[1] != exi.PayloadISO20Main || p.TermPT[0] != exi.PayloadISO20AC {
		t.Fatalf("pair %+v", p)
	}
	// Later loop requests (new time, new values) are answered by the device,
	// with its own time, and the EV reads the server's answer.
	for i, soc := range []int8{41, 55, 79} {
		e.now = e.now.Add(time.Duration(i+1) * time.Minute)
		ans, hit := deviceAnswer(t, p, e.loop(exi.ModeDynamic, soc, 10_900), e.now)
		if !hit {
			t.Fatalf("loop request #%d missed the cache", i+2)
		}
		d, err := exi.DecodeISO20Res(p.PT, ans)
		if err != nil {
			t.Fatal(err)
		}
		if d.TimeStamp != uint64(e.now.Unix()) || d.TargetPower.Float() != 11_000 || !bytes.Equal(d.SessionID, sid20) {
			t.Fatalf("cached answer %+v", d)
		}
	}
	// Other requests never match.
	pdStop, _, _ := exi.EncodeISO20Req(&exi.Request20{Kind: exi.K20PowerDelivery, SessionID: e.sid, TimeStamp: 1 << 30,
		EVProcessing: ptrTo(0), ChargeProgress: ptrTo(exi.Progress20Stop)})
	if _, hit := deviceAnswer(t, p, pdStop, e.now); hit {
		t.Fatal("PowerDeliveryReq matched the loop pair")
	}

	// A limit: new target, new key; the same limit again: the same key.
	key := p.Key
	e.cfg.LimitA = 8
	p2, _ := CachePairOf(e.st, e.cfg, e.now.Add(time.Hour))
	if p2.Key == key {
		t.Fatal("the limit did not change the pair")
	}
	d, _ := exi.DecodeISO20Res(p2.PT, p2.Res)
	if d.TargetPower.Float() != 8*230*3 {
		t.Fatalf("target under the limit %v", d.TargetPower.Float())
	}
	p3, _ := CachePairOf(e.st, e.cfg, e.now.Add(2*time.Hour))
	if p3.Key != p2.Key {
		t.Fatal("same answer, other time: the key changed")
	}
	// Termination scenario: Terminate in the loop, then PowerDelivery and
	// SessionStop, each with the time written by the device.
	for i, b := range p3.Term {
		out := append([]byte(nil), b...)
		if err := exi.WriteTimeStamp(out, p3.TermTS[i], 1_900_000_000); err != nil {
			t.Fatal(err)
		}
		d, err := exi.DecodeISO20Res(p3.TermPT[i], out)
		if err != nil || d.TimeStamp != 1_900_000_000 || exi.Failed20(d.ResponseCode) {
			t.Fatalf("term #%d: %+v %v", i, d, err)
		}
		if i == 0 && (d.Notification == nil || *d.Notification != exi.Notif20Terminate) {
			t.Fatalf("term #0 without Terminate: %+v", d)
		}
	}

	ok(t, e.send(&exi.Request20{Kind: exi.K20PowerDelivery, EVProcessing: ptrTo(0), ChargeProgress: ptrTo(exi.Progress20Stop)}))
	ok(t, e.send(&exi.Request20{Kind: exi.K20SessionStop, ChargingSession: ptrTo(exi.Session20Terminate)}))
	if e.st.Phase != PhaseEnded || e.st.EndReason != "terminated by EV" {
		t.Fatalf("end %s %s", e.st.Phase, e.st.EndReason)
	}
}

func TestISO20ScheduledRenegotiation(t *testing.T) {
	e := newEV20(t, "TLSv1.3", cfg20)
	e.setup(exi.ModeScheduled)
	r := e.stepLoop(e.loop(exi.ModeScheduled, 30, 11_000))
	if r.Notification != nil {
		t.Fatalf("notification without a change: %+v", r)
	}
	// The operator lowers the limit: the loop answer asks the EV to renegotiate.
	e.cfg.LimitA = 10
	r = e.stepLoop(e.loop(exi.ModeScheduled, 31, 11_000))
	if r.Notification == nil || *r.Notification != exi.Notif20ScheduleRenegotiation {
		t.Fatalf("no ScheduleRenegotiation: %+v", r)
	}
	ok(t, e.send(&exi.Request20{Kind: exi.K20PowerDelivery, EVProcessing: ptrTo(0), ChargeProgress: ptrTo(exi.Progress20ScheduleRenegotiation)}))
	se := e.send(&exi.Request20{Kind: exi.K20ScheduleExchange, MaxSupportingPoints: ptrTo(uint16(24)), ControlMode: exi.ModeScheduled})
	ok(t, se)
	if se.Schedule[0].Power.Float() != 10*230*3 {
		t.Fatalf("new schedule %+v", se.Schedule)
	}
	ok(t, e.send(&exi.Request20{Kind: exi.K20PowerDelivery, EVProcessing: ptrTo(0), ChargeProgress: ptrTo(exi.Progress20Start),
		ProfileMode: exi.ModeScheduled, SelectedScheduleTuple: ptrTo(uint32(1))}))
	if r = e.stepLoop(e.loop(exi.ModeScheduled, 32, 6_900)); r.Notification != nil {
		t.Fatalf("still renegotiating: %+v", r)
	}
}

// Without TLS 1.3 or with ISO20 off the handshake falls back to ISO 15118-2.
func TestISO20Fallback(t *testing.T) {
	for _, c := range []struct {
		tls string
		on  bool
	}{{"", true}, {"TLSv1.2 ECDHE-ECDSA-AES128-SHA256", true}, {"TLSv1.3", false}} {
		cf := cfg20
		cf.ISO20 = c.on
		e := newEV20(t, c.tls, cf)
		e.handshake()
		if e.st.Protocol != exi.NamespaceISO2 || e.st.SchemaID != 1 {
			t.Fatalf("tls %q iso20 %v: chose %s", c.tls, c.on, e.st.Protocol)
		}
	}
}

func TestISO20Errors(t *testing.T) {
	e := newEV20(t, "TLSv1.3", cfg20)
	e.handshake()
	e.sid = make([]byte, 8)
	r := e.send(&exi.Request20{Kind: exi.K20SessionSetup, EVCCID: "X"})
	e.sid = r.SessionID
	// out of sequence
	r = e.send(&exi.Request20{Kind: exi.K20ServiceDiscovery})
	if r.ResponseCode != exi.RC20FailedSequenceError || e.st.Phase != PhaseEnded {
		t.Fatalf("sequence error: %s, %s", r.ResponseName, e.st.Phase)
	}
	// unknown session
	e = newEV20(t, "TLSv1.3", cfg20)
	e.handshake()
	r = e.send(&exi.Request20{Kind: exi.K20SessionSetup, EVCCID: "X"})
	e.sid = []byte{1, 1, 1, 1, 1, 1, 1, 1}
	if r = e.send(&exi.Request20{Kind: exi.K20AuthorizationSetup}); r.ResponseCode != exi.RC20FailedUnknownSession {
		t.Fatalf("unknown session: %s", r.ResponseName)
	}
	// state lost: a mid-session -20 request resumes from SessionSetup and fails
	st := NewState(time.Now())
	b, pt, _ := exi.EncodeISO20Req(&exi.Request20{Kind: exi.K20ServiceDiscovery, SessionID: sid20, TimeStamp: 1 << 30})
	if !ResumePT(st, pt, b) || st.Protocol != exi.NamespaceISO20AC {
		t.Fatal("no resume of a -20 request")
	}
	res, err := StepPT(st, cfg20, DefaultEnv(), pt, b)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := exi.DecodeISO20Res(res.PT, res.EXI); d.ResponseCode != exi.RC20FailedUnknownSession || !res.Close {
		t.Fatalf("resume: %s close %v", d.ResponseName, res.Close)
	}
}
