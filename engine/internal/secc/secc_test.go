// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// testdata/josev_ac_eim_session.txt: the requests of a real Josev EVCC session
// (ISO 15118-2, AC, EIM) recorded on 30.10.2026 — type and EXI hex per line.
func loadVectors(t *testing.T) (types []string, msgs [][]byte) {
	f, err := os.Open("testdata/josev_ac_eim_session.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) != 2 {
			continue
		}
		b, err := hex.DecodeString(p[1])
		if err != nil {
			t.Fatal(err)
		}
		types, msgs = append(types, p[0]), append(msgs, b)
	}
	return
}

func testEnv(sid []byte) Env {
	return Env{Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
		Random: func(b []byte) { copy(b, sid) }}
}

var cfg = Config{EVSEID: "FI*CTR*ETEST", MaxCurrentA: 32, NominalVoltageV: 230, Phases: 3}

// The EV reuses the session ID the engine gave it; replay with the same one.
func recordedSessionID(t *testing.T, msgs [][]byte) []byte {
	q, err := exi.DecodeISO2(msgs[2])
	if err != nil {
		t.Fatal(err)
	}
	return q.SessionID
}

func TestJosevSessionReplay(t *testing.T) {
	types, msgs := loadVectors(t)
	if len(msgs) < 10 {
		t.Fatalf("only %d vectors", len(msgs))
	}
	env := testEnv(recordedSessionID(t, msgs))
	st := NewState(env.Now())
	for i, m := range msgs {
		r, err := Step(st, cfg, env, m)
		if err != nil {
			t.Fatalf("#%d %s: %v", i+1, types[i], err)
		}
		if r.RequestType != types[i] {
			t.Fatalf("#%d decoded as %s, recorded as %s", i+1, r.RequestType, types[i])
		}
		if res, ok := r.Response.(*exi.Response); ok {
			if res.ResponseCode != exi.RCOK && res.ResponseCode != exi.RCOKNewSession {
				t.Fatalf("#%d %s -> %s", i+1, r.RequestType, res.ResponseName)
			}
		}
		t.Logf("%-28s -> %-28s %3d B  phase %s", r.RequestType, r.ResponseType, len(r.EXI), st.Phase)
	}
	if st.Phase != PhaseEnded || st.EndReason != "terminated by EV" {
		t.Fatalf("final phase %s (%s)", st.Phase, st.EndReason)
	}
	if st.EV.EAmountWh == nil || *st.EV.EAmountWh != 60 || st.EV.EVMaxCurrentA == nil || *st.EV.EVMaxCurrentA != 32 {
		t.Fatalf("EV data %+v", st.EV)
	}
}

// Out-of-order request: the engine must answer FAILED_SequenceError and end.
func TestSequenceError(t *testing.T) {
	types, msgs := loadVectors(t)
	env := testEnv(recordedSessionID(t, msgs))
	st := NewState(env.Now())
	for i := 0; i < 3; i++ { // handshake, SessionSetup, ServiceDiscovery
		if _, err := Step(st, cfg, env, msgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	// skip PaymentServiceSelection and Authorization: send ChargeParameterDiscovery
	idx := -1
	for i, ty := range types {
		if ty == "ChargeParameterDiscoveryReq" {
			idx = i
		}
	}
	r, err := Step(st, cfg, env, msgs[idx])
	if err != nil {
		t.Fatal(err)
	}
	res := r.Response.(*exi.Response)
	if res.ResponseCode != exi.RCFailedSequenceError || st.Phase != PhaseEnded || !r.Close {
		t.Fatalf("got %s, phase %s, close %v", res.ResponseName, st.Phase, r.Close)
	}
}

// A partner limit lowers EVSEMaxCurrent, never above the station.
func TestLimit(t *testing.T) {
	c := cfg
	c.LimitA = 10
	if c.effectiveCurrent() != 10 {
		t.Fatal(c.effectiveCurrent())
	}
	c.LimitA = 63
	if c.effectiveCurrent() != 32 {
		t.Fatal("limit above the station must be ignored")
	}
}

// Cache: the recorded ChargingStatusReq are byte-identical within the session,
// the cached response equals the engine's own answer, a lower limit changes
// it, and the termination scenario decodes as StopCharging / OK / OK.
func TestCachePair(t *testing.T) {
	types, msgs := loadVectors(t)
	env := testEnv(recordedSessionID(t, msgs))
	st := NewState(env.Now())
	var firstReq []byte
	var pair []byte
	for i, m := range msgs {
		r, err := Step(st, cfg, env, m)
		if err != nil {
			t.Fatal(err)
		}
		if types[i] != "ChargingStatusReq" {
			continue
		}
		if firstReq == nil {
			firstReq = m
			if pair, _, err = CachePair(st, cfg); err != nil || pair == nil {
				t.Fatalf("pair: %v", err)
			}
		}
		if hex.EncodeToString(m) != hex.EncodeToString(firstReq) {
			t.Fatalf("#%d ChargingStatusReq differs within the session", i+1)
		}
		if hex.EncodeToString(r.EXI) != hex.EncodeToString(pair) {
			t.Fatalf("#%d engine answer differs from the cached pair", i+1)
		}
		// The device's one algorithm (ADR-005): ReqBits is the whole
		// request, no time stamp to write; it answers with the pair as is.
		p, err := CachePairOf(st, cfg, env.Now())
		if err != nil || p == nil || p.ReqBits != len(firstReq)*8 || p.TS != 0 || len(p.TermPT) != 3 {
			t.Fatalf("pair of ISO 15118-2: %+v %v", p, err)
		}
		if !exi.PrefixMatch(firstReq, m, p.ReqBits) || hex.EncodeToString(p.Res) != hex.EncodeToString(r.EXI) {
			t.Fatalf("#%d the device's prefix compare misses an ISO 15118-2 loop request", i+1)
		}
		if i+1 < len(types) && types[i+1] == "ChargingStatusReq" {
			continue
		}
		// last loop request: check the limit and the termination scenario here
		lim := cfg
		lim.LimitA = 10
		res10, term, err := CachePair(st, lim)
		if err != nil || hex.EncodeToString(res10) == hex.EncodeToString(pair) || len(term) != 3 {
			t.Fatalf("limited pair: %v, %d term", err, len(term))
		}
		t.Logf("pair %d B, term %d+%d+%d B", len(pair), len(term[0]), len(term[1]), len(term[2]))
	}
	if firstReq == nil {
		t.Fatal("no ChargingStatusReq in the vectors")
	}
	if p, _, _ := CachePair(st, cfg); p != nil {
		t.Fatal("pair after the session ended")
	}
}
