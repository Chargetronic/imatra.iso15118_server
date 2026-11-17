// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package rules: the business rules of a device (docs/rules.md).
//
// The service layer gives each device an open set of rules in its registry
// message ("rules": {...}); without a layer the operator sets them
// (scripts/registry.sh). The server keeps the whole set as given and applies
// the keys it has a handler for. A missing key means the default behaviour; a
// key of the wrong type is skipped (Invalid) and the others still apply; a
// key with no handler is kept and shown and changes nothing. A new rule is a
// new handler and a line in the catalogue: neither the schema nor the device
// protocol changes. No rule names a product or a device type.
package rules

import (
	"encoding/json"
	"sort"
)

// Topology values.
const (
	Inline     = "inline"     // between a charging station and the EV: masks the station's PWM
	Integrated = "integrated" // in the charging station: sets the PWM itself
)

// Supply: windows of charging that keep the device's own battery charged
// (a device powered by a battery it can charge only while current flows to
// the EV). Thresholds are the layer's: they change with the battery and
// the device's revision.
type Supply struct {
	BelowPct    float64 // open a window when the device's battery is below this
	MinCurrentA float64 // the EV's current during the window (6 A at least, IEC 61851-1)
	Minutes     int     // length of a window
}

// Set: the rules of one device, parsed.
type Set struct {
	raw map[string]json.RawMessage

	Topology           string   // Inline (default) or Integrated
	RatedCurrentA      *float64 // the device's rating: never offered more
	InstallMaxCurrentA *float64 // the installation's limit (breaker)
	Phases             *int     // 1 or 3
	Meter              string   // "clamp" (default) or "certified"
	Supply             *Supply
	Invalid            []string // keys of a known rule with a value it cannot use
}

// Parse reads a rules object; nil or invalid JSON gives the defaults.
func Parse(b []byte) Set {
	s := Set{Topology: Inline, Meter: "clamp", raw: map[string]json.RawMessage{}}
	if len(b) == 0 || json.Unmarshal(b, &s.raw) != nil {
		s.raw = map[string]json.RawMessage{}
		return s
	}
	bad := func(k string) { s.Invalid = append(s.Invalid, k) }
	if v, ok := s.raw["topology"]; ok {
		var t string
		if json.Unmarshal(v, &t) != nil || (t != Inline && t != Integrated) {
			bad("topology")
		} else {
			s.Topology = t
		}
	}
	num := func(k string, lo, hi float64) *float64 {
		v, ok := s.raw[k]
		if !ok {
			return nil
		}
		var f float64
		if json.Unmarshal(v, &f) != nil || f < lo || f > hi {
			bad(k)
			return nil
		}
		return &f
	}
	s.RatedCurrentA = num("rated_current_a", 6, 1000)
	s.InstallMaxCurrentA = num("install_max_current_a", 6, 1000)
	if f := num("phases", 1, 3); f != nil {
		if *f != 1 && *f != 3 {
			bad("phases")
		} else {
			p := int(*f)
			s.Phases = &p
		}
	}
	if v, ok := s.raw["meter"]; ok {
		var m string
		if json.Unmarshal(v, &m) != nil || (m != "clamp" && m != "certified") {
			bad("meter")
		} else {
			s.Meter = m
		}
	}
	if v, ok := s.raw["supply"]; ok {
		var x struct {
			BelowPct    *float64 `json:"below_pct"`
			MinCurrentA *float64 `json:"min_current_a"`
			Minutes     *int     `json:"minutes"`
		}
		if json.Unmarshal(v, &x) != nil || x.BelowPct == nil || *x.BelowPct <= 0 || *x.BelowPct >= 100 {
			bad("supply")
		} else {
			sp := Supply{BelowPct: *x.BelowPct, MinCurrentA: 6, Minutes: 20}
			if x.MinCurrentA != nil && *x.MinCurrentA >= 6 && *x.MinCurrentA <= 80 {
				sp.MinCurrentA = *x.MinCurrentA
			}
			if x.Minutes != nil && *x.Minutes >= 1 && *x.Minutes <= 240 {
				sp.Minutes = *x.Minutes
			}
			s.Supply = &sp
		}
	}
	sort.Strings(s.Invalid)
	return s
}

// Keys: every key given, known or not (shown to the operator as given).
func (s Set) Keys() []string {
	k := make([]string, 0, len(s.raw))
	for n := range s.raw {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// Known: the keys this server has a handler for. "topology" is validated and
// kept; the engine treats both topologies alike until a station with the
// module built in needs its own handling.
var Known = map[string]bool{"topology": true, "rated_current_a": true, "install_max_current_a": true,
	"phases": true, "meter": true, "supply": true}

// MaxCurrent: the station limit lowered by the device's rating and the
// installation's limit.
func (s Set) MaxCurrent(station float64) float64 {
	c := station
	for _, l := range []*float64{s.RatedCurrentA, s.InstallMaxCurrentA} {
		if l != nil && *l < c {
			c = *l
		}
	}
	return c
}

// SupplyNeeded: a window should open for a battery at soc (percent).
func (s Set) SupplyNeeded(soc float64) bool { return s.Supply != nil && soc < s.Supply.BelowPct }
