// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package secc

import (
	"testing"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
)

// State lost (ADR-002): a request in the middle of a session the server does
// not know gets FAILED_UnknownSession (V2G2-460) and the connection closes
// (V2G2-539); a SessionSetupReq gets a new session (V2G2-756).
func TestResumeLostSession(t *testing.T) {
	types, msgs := loadVectors(t)
	env := testEnv(recordedSessionID(t, msgs))
	if Resume(NewState(env.Now()), msgs[0]) {
		t.Fatal("supportedAppProtocolReq taken for a lost session")
	}
	for i, ty := range types {
		if ty != "ChargingStatusReq" && ty != "PowerDeliveryReq" {
			continue
		}
		st := NewState(env.Now())
		if !Resume(st, msgs[i]) || st.Protocol != exi.NamespaceISO2 {
			t.Fatalf("%s: not resumed as ISO 15118-2 (%q)", ty, st.Protocol)
		}
		r, err := Step(st, cfg, env, msgs[i])
		if err != nil {
			t.Fatal(err)
		}
		res := r.Response.(*exi.Response)
		if res.ResponseCode != exi.RCFailedUnknownSession || !r.Close || r.ResponseType != ty[:len(ty)-3]+"Res" {
			t.Fatalf("%s: got %s (%s), close %v", ty, res.ResponseName, r.ResponseType, r.Close)
		}
		break
	}
	st := NewState(env.Now())
	if !Resume(st, msgs[1]) {
		t.Fatal("SessionSetupReq not resumed")
	}
	r, err := Step(st, cfg, env, msgs[1])
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Response.(*exi.Response); res.ResponseCode != exi.RCOKNewSession || r.Close {
		t.Fatalf("SessionSetup: got %s", res.ResponseName)
	}
}

func TestResumeLostDINSession(t *testing.T) {
	types, msgs := loadDIN(t)
	q, _ := exi.DecodeDIN(msgs[2])
	env := testEnv(q.SessionID)
	st := NewState(env.Now())
	if !Resume(st, msgs[3]) || st.Protocol != exi.NamespaceDIN {
		t.Fatalf("%s: not resumed as DIN (%q)", types[3], st.Protocol)
	}
	r, err := Step(st, cfg, env, msgs[3])
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Response.(*exi.DINResponse); res.ResponseCode != exi.RCFailedUnknownSession || !r.Close {
		t.Fatalf("%s: got %s", types[3], res.ResponseName)
	}
}
