// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// testdata/josev_din_soc_session.txt: the requests of a real Josev EVCC DIN
// 70121 session (DC, SoC read) recorded on 31.10.2026 on the test bench.
func loadDIN(t *testing.T) (types []string, msgs [][]byte) {
	f, err := os.Open("testdata/josev_din_soc_session.txt")
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

// The engine walks a DC-only EV to ChargeParameterDiscovery, keeps its SoC
// and DC data, answers StopCharging and ends the session.
func TestDINSoCReplay(t *testing.T) {
	types, msgs := loadDIN(t)
	if len(msgs) != 6 {
		t.Fatalf("%d vectors, want 6 (handshake .. ChargeParameterDiscovery)", len(msgs))
	}
	q, err := exi.DecodeDIN(msgs[2])
	if err != nil {
		t.Fatal(err)
	}
	env := testEnv(q.SessionID)
	st := NewState(env.Now())
	var last *Result
	for i, m := range msgs {
		r, err := Step(st, cfg, env, m)
		if err != nil {
			t.Fatalf("#%d %s: %v", i+1, types[i], err)
		}
		if r.RequestType != types[i] {
			t.Fatalf("#%d decoded as %s, recorded as %s", i+1, r.RequestType, types[i])
		}
		if res, ok := r.Response.(*exi.DINResponse); ok && res.ResponseCode > exi.RCOKNewSession {
			t.Fatalf("#%d %s -> %s", i+1, r.RequestType, res.ResponseName)
		}
		t.Logf("%-28s -> %-28s %3d B  phase %s", r.RequestType, r.ResponseType, len(r.EXI), st.Phase)
		last = r
	}
	if st.Protocol != exi.NamespaceDIN || st.Phase != PhaseEnded || !last.Close {
		t.Fatalf("protocol %s, phase %s, close %v", st.Protocol, st.Phase, last.Close)
	}
	if st.EV.SoC == nil || st.EV.EVMaxVoltageV == nil {
		t.Fatalf("EV data %+v", st.EV)
	}
	if st.EV.EAmountWh != nil { // EAmount is an AC field; DIN values have their own
		t.Fatal("DIN session must not fill e_amount_wh")
	}
	res := last.Response.(*exi.DINResponse)
	if res.EVSENotification != exi.DINNotifStopCharging {
		t.Fatalf("notification %d, want StopCharging", res.EVSENotification)
	}
	found := false
	for _, e := range last.Events {
		found = found || e.Type == "soc.read"
	}
	if !found {
		t.Fatal("no soc.read event")
	}
	t.Logf("SoC %d %%, EV max %.0f V", *st.EV.SoC, *st.EV.EVMaxVoltageV)
}

// A DIN request out of order ends the session with FAILED_SequenceError.
func TestDINSequenceError(t *testing.T) {
	_, msgs := loadDIN(t)
	q, _ := exi.DecodeDIN(msgs[2])
	env := testEnv(q.SessionID)
	st := NewState(env.Now())
	for i := 0; i < 3; i++ { // handshake, SessionSetup, ServiceDiscovery
		if _, err := Step(st, cfg, env, msgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Step(st, cfg, env, msgs[5]) // ChargeParameterDiscovery too early
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Response.(*exi.DINResponse); res.ResponseCode != exi.RCFailedSequenceError || st.Phase != PhaseEnded || !r.Close {
		t.Fatalf("got %s, phase %s", res.ResponseName, st.Phase)
	}
}
