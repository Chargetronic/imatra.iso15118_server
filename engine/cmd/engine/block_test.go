// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func TestBlocklistReplace(t *testing.T) {
	b := &blocklist{set: map[string]struct{}{}}
	if got := b.replace([]string{"t-b", "t-a"}); len(got) != 2 {
		t.Fatalf("first load: added %v", got)
	}
	if !b.has("t-a") || b.has("t-c") {
		t.Fatal("membership")
	}
	added := b.replace([]string{"t-a", "t-c"})
	if len(added) != 1 || added[0] != "t-c" {
		t.Fatalf("added %v, want [t-c]", added)
	}
	if b.has("t-b") {
		t.Fatal("unblocked device still in the set")
	}
	if p := blockedPayload(b.list()); p != "v1 t-a t-c" {
		t.Fatalf("payload %q", p)
	}
	if p := blockedPayload(nil); p != "v1" {
		t.Fatalf("empty payload %q", p)
	}
	var nilb *blocklist
	if nilb.has("x") {
		t.Fatal("nil list blocks")
	}
}

func TestDeviceOfTopic(t *testing.T) {
	for topic, want := range map[string]string{
		"dev/t-1/v2g/down": "t-1", "dev/t-1/config": "t-1", "svc/blocked": "",
		"other/e-1/run": "", "dev/x": "",
	} {
		if got := deviceOfTopic(topic); got != want {
			t.Errorf("%s: %q, want %q", topic, got, want)
		}
	}
}

// recClient records publishes; the rest of mqtt.Client is not used.
type recClient struct {
	mqtt.Client
	topics []string
}

func (r *recClient) Publish(topic string, _ byte, _ bool, _ interface{}) mqtt.Token {
	r.topics = append(r.topics, topic)
	return doneToken{}
}

func TestGuardedClientDropsBlocked(t *testing.T) {
	e := &engine{blocked: &blocklist{set: map[string]struct{}{"t-bad": {}}}}
	rc := &recClient{}
	g := guardedClient{rc, e}
	for _, tp := range []string{"dev/t-bad/v2g/down", "dev/t-bad/cl/push", "dev/t-ok/v2g/down", BlockedTopic} {
		tok := g.Publish(tp, 1, false, "x")
		if !tok.WaitTimeout(time.Millisecond) || tok.Error() != nil {
			t.Fatalf("%s: token not done", tp)
		}
	}
	if len(rc.topics) != 2 || rc.topics[0] != "dev/t-ok/v2g/down" || rc.topics[1] != BlockedTopic {
		t.Fatalf("published %v", rc.topics)
	}
}
