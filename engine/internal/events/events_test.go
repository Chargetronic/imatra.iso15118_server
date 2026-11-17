// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package events

import (
	"encoding/json"
	"testing"
)

// The plug-in id goes into the feed data; the event's own data is not changed.
func TestPayloadPlug(t *testing.T) {
	d := map[string]any{"cp": "B"}
	p := Event{Type: SessionStarted, Plug: "a1b2c3d4-3", Data: d}.Payload().(map[string]any)
	if p["plug"] != "a1b2c3d4-3" || p["cp"] != "B" {
		t.Fatalf("payload %v", p)
	}
	if _, has := d["plug"]; has {
		t.Fatal("the event's data was changed")
	}
	if got := (Event{Data: d}).Payload(); got.(map[string]any)["plug"] != nil {
		t.Fatalf("no plug: %v", got)
	}
	type s struct {
		A int `json:"a"`
	}
	b, _ := json.Marshal(Event{Plug: "x-1", Data: s{7}}.Payload())
	if string(b) != `{"a":7,"plug":"x-1"}` {
		t.Fatalf("struct data: %s", b)
	}
}
