// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package exi

import (
	"bytes"
	"testing"
)

var sid20 = []byte{1, 2, 3, 4, 5, 6, 7, 8}

const ts20 = 1_800_000_000_000_000 // microseconds (V2G20-1534)

func TestISO20RequestRoundTrip(t *testing.T) {
	cases := []*Request20{
		{Kind: K20SessionSetup, SessionID: make([]byte, 8), EVCCID: "WMIV1234567890ABCDEF"},
		{Kind: K20AuthorizationSetup},
		{Kind: K20Authorization, SelectedAuthorization: ptr(0)},
		{Kind: K20ServiceDiscovery},
		{Kind: K20ServiceDetail, ServiceID: ptr(uint16(ServiceAC))},
		{Kind: K20ServiceSelection, SelectedServiceID: ptr(uint16(1)), SelectedParameterSetID: ptr(uint16(2))},
		{Kind: K20ACChargeParamDisc, MaxPower: &Rat{11, 3}, MinPower: &Rat{1380, 0}},
		{Kind: K20ScheduleExchange, MaxSupportingPoints: ptr(uint16(24)), ControlMode: ModeDynamic,
			DepartureTime: ptr(uint32(7200)), TargetEnergy: &Rat{20, 3}, MaxEnergy: &Rat{40, 3}, MinEnergy: &Rat{5, 3}},
		{Kind: K20ScheduleExchange, MaxSupportingPoints: ptr(uint16(24)), ControlMode: ModeScheduled,
			TargetEnergy: &Rat{20, 3}},
		{Kind: K20PowerDelivery, EVProcessing: ptr(0), ChargeProgress: ptr(Progress20Start),
			ProfileMode: ModeScheduled, SelectedScheduleTuple: ptr(uint32(1))},
		{Kind: K20ACChargeLoop, ControlMode: ModeDynamic, PresentSoC: ptr(int8(42)), TargetEnergy: &Rat{18, 3},
			MaxEnergy: &Rat{40, 3}, MinEnergy: &Rat{0, 0}, MaxPower: &Rat{11, 3}, MinPower: &Rat{1380, 0},
			PresentPower: &Rat{7400, 0}},
		{Kind: K20ACChargeLoop, ControlMode: ModeScheduled, PresentPower: &Rat{3700, 0}},
		{Kind: K20SessionStop, ChargingSession: ptr(Session20Terminate)},
	}
	for _, q := range cases {
		if q.SessionID == nil {
			q.SessionID = sid20
		}
		q.TimeStamp = ts20
		b, pt, err := EncodeISO20Req(q)
		if err != nil {
			t.Fatalf("%v: encode: %v", q.Kind, err)
		}
		if pt != q.Kind.PayloadType() {
			t.Fatalf("%v: payload type 0x%04x", q.Kind, pt)
		}
		d, err := DecodeISO20(pt, b)
		if err != nil {
			t.Fatalf("%v: decode: %v", q.Kind, err)
		}
		if d.Kind != q.Kind || d.TimeStamp != ts20 || !bytes.Equal(d.SessionID, q.SessionID) {
			t.Fatalf("%v: got %v ts %d sid %x", q.Kind, d.Kind, d.TimeStamp, d.SessionID)
		}
		if q.ControlMode != 0 && d.ControlMode != q.ControlMode {
			t.Fatalf("%v: control mode %d, want %d", q.Kind, d.ControlMode, q.ControlMode)
		}
		if q.TargetEnergy != nil && (d.TargetEnergy == nil || *d.TargetEnergy != *q.TargetEnergy) {
			t.Fatalf("%v: target energy %v", q.Kind, d.TargetEnergy)
		}
		if q.PresentSoC != nil && (d.PresentSoC == nil || *d.PresentSoC != *q.PresentSoC) {
			t.Fatalf("%v: SoC %v", q.Kind, d.PresentSoC)
		}
		t.Logf("%-30s pt 0x%04x %3d B", d.Type, pt, len(b))
	}
}

func TestISO20ResponseRoundTrip(t *testing.T) {
	notif := Notif20ScheduleRenegotiation
	cases := []*Response20{
		{Kind: K20SessionSetup, ResponseCode: RC20OKNewSession, EVSEID: "FI*CTR*E0001"},
		{Kind: K20AuthorizationSetup},
		{Kind: K20Authorization},
		{Kind: K20ServiceDiscovery, Services: []uint16{ServiceAC}},
		{Kind: K20ServiceDetail, ServiceID: ServiceAC, ParamSets: []ParamSet{
			{ID: 1, Connector: 2, ControlMode: ModeDynamic, NominalVoltage: 230, MobilityNeeds: 1},
			{ID: 2, Connector: 2, ControlMode: ModeScheduled, NominalVoltage: 230, MobilityNeeds: 1}}},
		{Kind: K20ServiceSelection},
		{Kind: K20ACChargeParamDisc, EVSEMaxPower: &Rat{22, 3}, EVSEMinPower: &Rat{4140, 0}, EVSENominalFrequency: &Rat{50, 0}},
		{Kind: K20ScheduleExchange, ControlMode: ModeDynamic},
		{Kind: K20ScheduleExchange, ControlMode: ModeScheduled, ScheduleTuple: 1, TimeAnchor: ts20,
			Schedule: []PowerEntry{{DurationS: 86400, Power: Rat{22, 3}}}},
		{Kind: K20PowerDelivery},
		{Kind: K20ACChargeLoop, ControlMode: ModeDynamic, TargetPower: &Rat{11, 3}},
		{Kind: K20ACChargeLoop, ControlMode: ModeScheduled, Notification: &notif, NotificationMaxDelay: 60},
		{Kind: K20SessionStop},
	}
	for _, s := range cases {
		s.SessionID, s.TimeStamp = sid20, ts20
		b, pt, err := EncodeISO20Res(s)
		if err != nil {
			t.Fatalf("%v: encode: %v", s.Kind, err)
		}
		d, err := DecodeISO20Res(pt, b)
		if err != nil {
			t.Fatalf("%v: decode: %v", s.Kind, err)
		}
		if d.Kind != s.Kind || d.ResponseCode != s.ResponseCode || d.TimeStamp != ts20 {
			t.Fatalf("%v: got %v %s ts %d", s.Kind, d.Kind, d.ResponseName, d.TimeStamp)
		}
		switch s.Kind {
		case K20ServiceDetail:
			if len(d.ParamSets) != 2 || d.ParamSets[0] != s.ParamSets[0] {
				t.Fatalf("parameter sets %+v", d.ParamSets)
			}
		case K20ScheduleExchange:
			if d.ControlMode != s.ControlMode || len(d.Schedule) != len(s.Schedule) {
				t.Fatalf("schedule %+v", d)
			}
		case K20ACChargeLoop:
			if d.ControlMode != s.ControlMode || (s.TargetPower != nil && *d.TargetPower != *s.TargetPower) ||
				(s.Notification != nil && (d.Notification == nil || *d.Notification != notif)) {
				t.Fatalf("loop %+v", d)
			}
		}
		// The time stamp field of every response is found, and writing a new
		// time there gives exactly the message encoded with that time.
		at, err := TimeStampBit(pt, b, true)
		if err != nil {
			t.Fatalf("%v: time stamp bit: %v", s.Kind, err)
		}
		const later = ts20 + 3_601
		patched := append([]byte(nil), b...)
		if err := WriteTimeStamp(patched, at, later); err != nil {
			t.Fatal(err)
		}
		s2 := *s
		s2.TimeStamp = later
		want, _, err := EncodeISO20Res(&s2)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(patched, want) {
			t.Fatalf("%v: patched %x, encoded %x", s.Kind, patched, want)
		}
		t.Logf("%-30s pt 0x%04x %3d B  time stamp at bit %d", d.Type, pt, len(b), at)
	}
}

// The device's cache test: requests of one session match on the prefix
// (message type + session ID) whatever their time stamp and values; other
// message types and other sessions do not.
func TestISO20LoopPrefix(t *testing.T) {
	loop := func(sid []byte, ts uint64, w int16, soc int8) []byte {
		b, _, err := EncodeISO20Req(&Request20{Kind: K20ACChargeLoop, SessionID: sid, TimeStamp: ts,
			ControlMode: ModeDynamic, PresentSoC: &soc, TargetEnergy: &Rat{18, 3}, MaxEnergy: &Rat{40, 3},
			MinEnergy: &Rat{0, 0}, MaxPower: &Rat{11, 3}, MinPower: &Rat{1380, 0}, PresentPower: &Rat{w, 0}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := loop(sid20, ts20, 7400, 40)
	bits, err := TimeStampBit(PayloadISO20AC, first, false)
	if err != nil {
		t.Fatal(err)
	}
	if bits < 64 {
		t.Fatalf("prefix of %d bits cannot hold the session id", bits)
	}
	for _, other := range [][]byte{loop(sid20, ts20+1, 7300, 41), loop(sid20, ts20+86_400, 0, 80), loop(sid20, 5, 1, 1)} {
		if !PrefixMatch(first, other, bits) {
			t.Fatalf("a loop request of the same session does not match")
		}
	}
	if PrefixMatch(first, loop([]byte{9, 9, 9, 9, 9, 9, 9, 9}, ts20, 7400, 40), bits) {
		t.Fatal("another session matched")
	}
	cpd, _, err := EncodeISO20Req(&Request20{Kind: K20ACChargeParamDisc, SessionID: sid20, TimeStamp: ts20,
		MaxPower: &Rat{11, 3}, MinPower: &Rat{1380, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if PrefixMatch(first, cpd, bits) {
		t.Fatal("another message type matched")
	}
	t.Logf("prefix: %d bits of %d B", bits, len(first))
}

func TestWriteTimeStampRange(t *testing.T) {
	b := make([]byte, 8)
	if WriteTimeStamp(b, 0, TimeStampMin-1) == nil || WriteTimeStamp(b, 0, TimeStampMax+1) == nil || WriteTimeStamp(b, 30, ts20) == nil {
		t.Fatal("out-of-range time or field accepted")
	}
}

func TestTimeStampWriteRead(t *testing.T) {
	b := make([]byte, 12)
	for _, ts := range []uint64{TimeStampMin, ts20, TimeStampMax} {
		if err := WriteTimeStamp(b, 5, ts); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadTimeStamp(b, 5); err != nil || got != ts {
			t.Fatalf("%d: read %d %v", ts, got, err)
		}
	}
}
