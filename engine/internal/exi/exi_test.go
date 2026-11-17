// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package exi

import (
	"encoding/hex"
	"testing"
)

// supportedAppProtocolReq offering DIN 70121 (well-known sample from pyPLC).
const dinAppHand = "8000dbab9371d3234b71d1b981899189d191818991d26b9b3a232b30020000040040"

func TestDecodeAppHand(t *testing.T) {
	b, _ := hex.DecodeString(dinAppHand)
	ps, err := DecodeAppHand(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Namespace != NamespaceDIN {
		t.Fatalf("unexpected %+v", ps)
	}
	t.Logf("%+v", ps)
}

func TestEncodeAppHandRes(t *testing.T) {
	b, err := EncodeAppHandRes(AppHandOK, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("res % x", b)
}

func TestEncodeAll(t *testing.T) {
	sid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	v := PV{Value: 230, Unit: UnitV}
	c := PV{Value: 32, Unit: UnitA}
	d := uint32(86400)
	for _, r := range []*Response{
		{Kind: KindSessionSetup, SessionID: sid, ResponseCode: RCOKNewSession, EVSEID: "FI*CTR*E0001"},
		{Kind: KindServiceDiscovery, SessionID: sid, PaymentOptions: []int{1}, ServiceID: 1, FreeService: true, EnergyModes: []int{ModeACSingle, ModeACThree}},
		{Kind: KindPaymentServiceSelection, SessionID: sid},
		{Kind: KindAuthorization, SessionID: sid},
		{Kind: KindChargeParameterDiscovery, SessionID: sid, Schedules: []ScheduleTuple{{ID: 1, Entries: []PMaxEntry{{Start: 0, Duration: &d, PMax: MakePV(22080, UnitW, 0)}}}}, EVSENominalVoltage: &v, EVSEMaxCurrent: &c},
		{Kind: KindPowerDelivery, SessionID: sid},
		{Kind: KindChargingStatus, SessionID: sid, EVSEID: "FI*CTR*E0001", SAScheduleTupleID: 1, EVSEMaxCurrent: &c},
		{Kind: KindSessionStop, SessionID: sid},
	} {
		b, err := EncodeISO2Res(r)
		if err != nil {
			t.Fatalf("%v: %v", r.Kind, err)
		}
		t.Logf("%-34s %3d bytes", r.Type, len(b))
	}
}

func TestMakePV(t *testing.T) {
	p := MakePV(22080, UnitW, 0)
	if p.Float() != 22080 {
		t.Fatalf("%v", p)
	}
	p = MakePV(60000, UnitWh, 0)
	if p.Float() != 60000 || p.Multiplier != 1 {
		t.Fatalf("%v", p)
	}
}
