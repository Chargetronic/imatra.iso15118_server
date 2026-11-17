// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package evexi

import (
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
)

// A full AC EIM session generated here is accepted by the engine's state
// machine, and every response is readable on the EV side.
func TestSessionAgainstEngine(t *testing.T) {
	cfg := secc.Config{EVSEID: "FI*CTR*ETEST", MaxCurrentA: 32, NominalVoltageV: 230, Phases: 3}
	env := secc.Env{Now: time.Now, Random: func(b []byte) {
		for i := range b {
			b[i] = byte(i + 1)
		}
	}}
	st := secc.NewState(time.Now())

	ah, err := EncodeAppHandReq()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secc.Step(st, cfg, env, ah); err != nil {
		t.Fatalf("apphand: %v", err)
	}

	var sid []byte
	step := func(r *Req) *Res {
		t.Helper()
		r.SessionID = sid
		b, err := EncodeReq(r)
		if err != nil {
			t.Fatal(err)
		}
		res, err := secc.Step(st, cfg, env, b)
		if err != nil {
			t.Fatalf("%v: %v", r.Kind, err)
		}
		d, err := DecodeRes(res.EXI)
		if err != nil {
			t.Fatalf("%v response: %v", r.Kind, err)
		}
		if d.Kind != r.Kind {
			t.Fatalf("%v: got %v", r.Kind, d.Kind)
		}
		if d.ResponseCode > 1 { // OK or OK_NewSessionEstablished
			t.Fatalf("%v: response code %d", r.Kind, d.ResponseCode)
		}
		return d
	}

	d := step(&Req{Kind: SessionSetup, EVCCID: [6]byte{2, 0, 0, 0, 0, 1}})
	sid = d.SessionID
	if len(sid) != 8 {
		t.Fatalf("session id %x", sid)
	}
	step(&Req{Kind: ServiceDiscovery})
	step(&Req{Kind: PaymentServiceSelection})
	for i := 0; ; i++ {
		if !step(&Req{Kind: Authorization}).Ongoing || i > 20 {
			break
		}
	}
	var cpd *Res
	for i := 0; ; i++ {
		cpd = step(&Req{Kind: ChargeParameterDiscovery, DepartureS: 8 * 3600, EAmountWh: 20000, MaxCurrentA: 16})
		if !cpd.Ongoing || i > 20 {
			break
		}
	}
	if cpd.SATupleID == 0 {
		t.Fatalf("no SAScheduleTuple")
	}
	step(&Req{Kind: PowerDelivery, ChargeProgress: 0, SATupleID: uint8(cpd.SATupleID)})
	cs := step(&Req{Kind: ChargingStatus})
	t.Logf("charging status: max %.1f A (%v), notification %d", cs.MaxCurrentA, cs.HasMaxCurrent, cs.Notification)
	step(&Req{Kind: PowerDelivery, ChargeProgress: 1, SATupleID: uint8(cpd.SATupleID)})
	step(&Req{Kind: SessionStop})
}
