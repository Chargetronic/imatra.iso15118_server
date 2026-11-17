// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// memState: the stateStore in memory (OSS scope, task B2). The engine's tests
// run on it without Valkey and without Postgres; stateContract below checks
// that it and the Valkey store (VALKEY_TEST_ADDR) behave the same, so it is
// also the reference for another implementation of the extension point.

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

type memSession struct {
	dev, conn string
	st        []byte // state JSON: a saved state is never shared with the caller
	ver       int64
	ended     bool
	fin       bool // v2g.ended written
}

type memPower struct {
	boot string
	seq  int64
	w    float64
	set  bool
}

type memState struct {
	mu     sync.Mutex
	s      map[string]*memSession // session id ->
	byConn map[string]string      // device|conn -> session id
	open   map[string]string      // device -> newest open session id
	pw     map[string]*memPower
	events []events.Event
}

func newMemState() *memState {
	return &memState{s: map[string]*memSession{}, byConn: map[string]string{}, open: map[string]string{},
		pw: map[string]*memPower{}}
}

func (m *memState) session(id string, x *memSession) *store.Session {
	st := &secc.State{}
	_ = json.Unmarshal(x.st, st)
	return &store.Session{ID: id, State: st, Ver: x.ver}
}

func (m *memState) Load(_ context.Context, device, conn string) (*store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byConn[device+"|"+conn]
	if !ok {
		return nil, nil
	}
	x := m.s[id]
	if x.ended {
		return nil, errEnded
	}
	return m.session(id, x), nil
}

func (m *memState) Create(_ context.Context, device, conn string, st *secc.State) (*store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byConn[device+"|"+conn]; ok { // created meanwhile (as Valkey's claim)
		return m.session(id, m.s[id]), nil
	}
	raw, _ := json.Marshal(st)
	id := uuid4()
	m.s[id] = &memSession{dev: device, conn: conn, st: raw, ver: 1}
	m.byConn[device+"|"+conn] = id
	m.open[device] = id
	return m.session(id, m.s[id]), nil
}

func (m *memState) Save(_ context.Context, s *store.Session, evs ...events.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.s[s.ID]
	if !ok || x.ver != s.Ver || x.ended {
		return errConflict
	}
	x.st, _ = json.Marshal(s.State)
	x.ver++
	s.Ver = x.ver
	if events.Ends(evs) {
		x.fin = true
	}
	m.events = append(m.events, evs...)
	return nil
}

func (m *memState) end(id, reason string) {
	x := m.s[id]
	if x == nil {
		return
	}
	x.ended = true
	x.ver++
	if !x.fin {
		x.fin = true
		m.events = append(m.events, events.Event{Type: events.V2GEnded, Device: x.dev, SessionID: id,
			Time: time.Now().UTC(), Data: map[string]any{"reason": reason}})
	}
	if m.open[x.dev] == id {
		delete(m.open, x.dev)
	}
}

func (m *memState) Close(_ context.Context, device, conn, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byConn[device+"|"+conn]; ok {
		m.end(id, reason)
	}
	return nil
}

func (m *memState) CloseAll(_ context.Context, device, reason string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.open[device]
	if !ok {
		return 0, nil
	}
	m.end(id, reason)
	return 1, nil
}

func (m *memState) Open(_ context.Context, device string) ([]store.OpenSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.OpenSession
	for d, id := range m.open {
		if device != "" && d != device {
			continue
		}
		x := m.s[id]
		out = append(out, store.OpenSession{Device: d, Conn: x.conn, Session: *m.session(id, x)})
	}
	return out, nil
}

func (m *memState) Emit(_ context.Context, evs ...events.Event) error {
	m.mu.Lock()
	m.events = append(m.events, evs...)
	m.mu.Unlock()
	return nil
}

func (m *memState) Power(_ context.Context, device, sid string, ord order, at time.Time, plug string, w, _ float64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pw[device]
	if p == nil {
		p = &memPower{}
		m.pw[device] = p
	}
	if p.boot == ord.boot && p.seq >= ord.seq && (p.seq != 0 || p.boot != "") {
		return false, nil // an older reading of the same boot
	}
	p.boot, p.seq = ord.boot, ord.seq
	w = math.Round(w)
	if p.set && math.Abs(p.w-w) < powerStep {
		return false, nil
	}
	p.w, p.set = w, true
	m.events = append(m.events, powerEvent(device, sid, at, plug, w))
	return true, nil
}

func (m *memState) types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.events {
		out = append(out, e.Type)
	}
	return out
}
