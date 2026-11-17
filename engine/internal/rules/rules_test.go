// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package rules

import (
	"reflect"
	"testing"
)

func TestDefaults(t *testing.T) {
	for _, b := range [][]byte{nil, []byte(`{}`), []byte(`not json`)} {
		s := Parse(b)
		if s.Topology != Inline || s.Meter != "clamp" || s.RatedCurrentA != nil || s.Supply != nil || len(s.Invalid) != 0 {
			t.Fatalf("%q: %+v", b, s)
		}
		if s.MaxCurrent(32) != 32 || s.SupplyNeeded(1) {
			t.Fatalf("%q: no rules must change nothing", b)
		}
	}
}

func TestKnownUnknownInvalid(t *testing.T) {
	s := Parse([]byte(`{"topology":"integrated","rated_current_a":16,"install_max_current_a":25,"phases":1,
		"meter":"certified","supply":{"below_pct":20,"min_current_a":8,"minutes":15},
		"future_rule":{"x":1},"v2g":{"hw":true}}`))
	if s.Topology != Integrated || *s.RatedCurrentA != 16 || *s.InstallMaxCurrentA != 25 || *s.Phases != 1 ||
		s.Meter != "certified" || *s.Supply != (Supply{20, 8, 15}) || len(s.Invalid) != 0 {
		t.Fatalf("%+v", s)
	}
	if s.MaxCurrent(32) != 16 || s.MaxCurrent(10) != 10 {
		t.Fatal("max current: the lowest of station, rating, installation")
	}
	if !s.SupplyNeeded(19.5) || s.SupplyNeeded(20) {
		t.Fatal("supply threshold")
	}
	if !reflect.DeepEqual(s.Keys(), []string{"future_rule", "install_max_current_a", "meter", "phases",
		"rated_current_a", "supply", "topology", "v2g"}) {
		t.Fatalf("keys kept as given: %v", s.Keys())
	}
	if Known["future_rule"] || Known["v2g"] {
		t.Fatal("no handler for unknown keys")
	}

	bad := Parse([]byte(`{"topology":"wall","rated_current_a":"16","phases":2,"meter":7,"supply":{"min_current_a":8},
		"install_max_current_a":20}`))
	if !reflect.DeepEqual(bad.Invalid, []string{"meter", "phases", "rated_current_a", "supply", "topology"}) {
		t.Fatalf("invalid: %v", bad.Invalid)
	}
	if bad.Topology != Inline || bad.RatedCurrentA != nil || bad.Phases != nil || bad.Supply != nil ||
		bad.MaxCurrent(32) != 20 {
		t.Fatalf("a bad key is skipped, the others apply: %+v", bad)
	}
	d := Parse([]byte(`{"supply":{"below_pct":30}}`))
	if *d.Supply != (Supply{30, 6, 20}) {
		t.Fatalf("supply defaults: %+v", d.Supply)
	}
}
