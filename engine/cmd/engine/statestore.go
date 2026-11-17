// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Where the state of running V2G sessions lives: Valkey, the only store of
// the working state (ADR-002; OSS scope 13.11.2026, task B1 — the earlier
// single-engine mode on Postgres is gone). A step reads the state from
// Valkey and writes it back with a version check (compare-and-set in a Lua
// script), so several engine instances can serve the same devices without
// overwriting each other. Postgres gets nothing from the hot path: the
// sessions rows and every operator table come from the feed events (relay,
// projector). The interface stays as an extension point; tests
// use an implementation in memory (memstate_test.go).
//
// Keys (all expire after two days without a write):
//
//	s:<session_id>     hash: dev, conn, st (state JSON), v (version), ended
//	c:<device>:<conn>  session id of a V2G connection
//	o:<device>         hash: sid, conn — the device's newest open session
//	open               set of devices with an open session (leader jobs)
//	events             stream of the operator's events, read by the relay
//	dseq:<device>      per-device event sequence (ADR-001); no expiry
//	pw:<device>        hash p, b, n: last reported power, boot and number of its reading (ADR-003)
//
// Events: a step's events go into the stream in the same Lua call
// that saves its state — both or neither. Events not tied to a session step
// (presence, CP, power) go in with their own small Lua call.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

var errConflict = errors.New("session state changed meanwhile")

// errEnded: the session was closed (EV unplugged or connection closed),
// possibly by another instance; a late request of it gets no answer.
var errEnded = errors.New("session already ended")

type stateStore interface {
	Load(ctx context.Context, device, conn string) (*store.Session, error) // nil, nil: none
	Create(ctx context.Context, device, conn string, st *secc.State) (*store.Session, error)
	// Save the state after a step together with the events it produced.
	Save(ctx context.Context, s *store.Session, evs ...events.Event) error // errConflict: reload and redo
	// Close and CloseAll end sessions; one that was still running gets its
	// v2g.ended event here.
	Close(ctx context.Context, device, conn, reason string) error
	CloseAll(ctx context.Context, device, reason string) (int64, error)
	Open(ctx context.Context, device string) ([]store.OpenSession, error) // "" = all devices
	// Emit writes events not tied to a session step.
	Emit(ctx context.Context, evs ...events.Event) error
	// Power reports power.changed when the power moved by the threshold
	// since the last report and the reading is newer than the last one.
	// Power: a metering report (power W, the device's energy counter Wh).
	Power(ctx context.Context, device, sessionID string, ord order, at time.Time, plug string, w, eWh float64) (bool, error)
}

// powerStep: threshold of power.changed, W (POWER_CHANGE_W).
var powerStep = envF("POWER_CHANGE_W", 300)

func newStateStore(db *store.Store, log *slog.Logger) (stateStore, error) {
	addr := env("VALKEY_ADDR", "valkey:6379")
	c := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("VALKEY_PASSWORD"),
		PoolSize: 64, MinIdleConns: 8})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("valkey %s: %w", addr, err)
	}
	v := &vkState{c: c, db: db, log: log,
		// The relay enforces EVENTS_STREAM_MAX and reports what it had to
		// cut; this cap only protects Valkey while no relay runs.
		maxLen: strconv.Itoa(int(envF("EVENTS_STREAM_MAX", 500_000) * 1.25))}
	log.Info("session state in Valkey", "addr", addr)
	return v, nil
}

// order: of a device's meter readings, by its boot and its message number
// (ADR-003); a reading of another boot replaces the last one.
type order struct {
	boot string
	seq  int64
}

func powerEvent(device, sid string, at time.Time, plug string, w float64) events.Event {
	return events.Event{Type: events.PowerChanged, Device: device, SessionID: sid, Time: at, Plug: plug,
		Data: map[string]any{"power_w": math.Round(w), "quality": "measured"}}
}

// ------------------------------------------------------------------ Valkey

type vkState struct {
	c   *redis.Client
	db  *store.Store
	log *slog.Logger

	maxLen string // emergency cap of the stream (approximate trim), 1.25 x EVENTS_STREAM_MAX
}

const keyTTL = 48 * time.Hour

// luaEmit: shared by the scripts below. dseq starts, when missing, from the
// current time in s x 1000, so it keeps growing even if Valkey lost it (as
// long as a device has fewer than 1000 events a second). 13 digits: exact in
// Lua's number-to-string conversion (%.14g).
var luaEmit = luaHot + `
local function emit(stream, dkey, maxlen, dev, sid, typ, ts, data)
  if redis.call('EXISTS', dkey) == 0 then
    redis.call('SET', dkey, redis.call('TIME')[1] .. '000')
  end
  if typ == '` + events.LimitChanged + `' and limitSame(dev, data) then return end
  local q = redis.call('INCR', dkey)
  if typ == '` + events.V2GEnded + `' then data = v2gEnergy(dev, data) end
  redis.call('XADD', stream, 'MAXLEN', '~', maxlen, '*',
    'd', dev, 's', sid, 't', typ, 'ts', ts, 'v', '` + evVer + `', 'q', q, 'j', data)
  hot(dev, typ, data)
end
`

var evVer = strconv.Itoa(events.SchemaVer)

// casSave: KEYS = s:<id>, events, dseq:<dev>; ARGV = expected version, state
// JSON, ttl s, stream cap, device, session id, ends (1: the events close the
// session), then type, time, data per event.
// Returns the new version, or -1 when the version moved on, the key is gone
// or the session has ended; no event is written then.
var casSave = redis.NewScript(luaEmit + `
local v = redis.call('HGET', KEYS[1], 'v')
if not v or tonumber(v) ~= tonumber(ARGV[1]) then return -1 end
if redis.call('HGET', KEYS[1], 'ended') == '1' then return -1 end
local nv = tonumber(v) + 1
redis.call('HSET', KEYS[1], 'st', ARGV[2], 'v', nv)
if ARGV[7] == '1' then redis.call('HSET', KEYS[1], 'fin', 1) end
redis.call('EXPIRE', KEYS[1], ARGV[3])
for i = 8, #ARGV, 3 do
  emit(KEYS[2], KEYS[3], ARGV[4], ARGV[5], ARGV[6], ARGV[i], ARGV[i+1], ARGV[i+2])
end
return nv`)

// closeScript: KEYS = s:<id>, events, dseq:<dev>; ARGV = stream cap, device,
// session id, time, data. Ended flag and a new version in one step (a step of
// this session on another instance then fails its compare-and-set, ADR-001);
// v2g.ended unless the session already reported its end.
var closeScript = redis.NewScript(luaEmit + `
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('HSET', KEYS[1], 'ended', 1)
redis.call('HINCRBY', KEYS[1], 'v', 1)
if redis.call('HGET', KEYS[1], 'fin') == '1' then return 1 end
redis.call('HSET', KEYS[1], 'fin', 1)
emit(KEYS[2], KEYS[3], ARGV[1], ARGV[2], ARGV[3], '` + events.V2GEnded + `', ARGV[4], ARGV[5])
return 2`)

// emitScript: KEYS = events, dseq:<dev>; ARGV = stream cap, device, then
// session id, type, time, data per event.
var emitScript = redis.NewScript(luaEmit + `
for i = 3, #ARGV, 4 do
  emit(KEYS[1], KEYS[2], ARGV[1], ARGV[2], ARGV[i], ARGV[i+1], ARGV[i+2], ARGV[i+3])
end
return 1`)

// powerScript: KEYS = events, dseq:<dev>, pw:<dev>; ARGV = stream cap,
// device, session id, reading number, power W, threshold W, time, data,
// energy counter Wh, boot. Within a boot an older reading than the last one
// is ignored; another boot replaces it (ADR-003, no device clock); the energy of the plug-in grows by the
// counter's non-negative increments (it restarts at 0 with the device) in the
// device's hot record (hs: eplug, pool.go), so the unplug reads it from
// Valkey (ADR-002); power.changed only when the power moved by the threshold
// since the last report.
var powerScript = redis.NewScript(luaEmit + `
local last = redis.call('HMGET', KEYS[3], 'b', 'n')
if last[1] == ARGV[10] and last[2] and tonumber(last[2]) >= tonumber(ARGV[4]) then return 0 end
local p = redis.call('HGET', KEYS[3], 'p')
redis.call('HSET', KEYS[3], 'b', ARGV[10], 'n', ARGV[4])
redis.call('EXPIRE', KEYS[3], 172800)
local hk = 'hs:' .. ARGV[2]
local el = redis.call('HGET', hk, 'elast')
if el then
  local d = tonumber(ARGV[9]) - tonumber(el)
  if d > 0 then redis.call('HINCRBYFLOAT', hk, 'eplug', d) end
end
redis.call('HSET', hk, 'elast', ARGV[9])
redis.call('EXPIRE', hk, 172800)
if p and math.abs(tonumber(p) - tonumber(ARGV[5])) < tonumber(ARGV[6]) then return 0 end
redis.call('HSET', KEYS[3], 'p', ARGV[5])
emit(KEYS[1], KEYS[2], ARGV[1], ARGV[2], ARGV[3], '` + events.PowerChanged + `', ARGV[7], ARGV[8])
return 1`)

var streamKey = "events" // a var for the tests

func dseqKey(d string) string { return "dseq:" + d }
func pwKey(d string) string   { return "pw:" + d }
func evTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func evData(d any) string {
	if d == nil {
		return "{}"
	}
	b, _ := json.Marshal(d)
	return string(b)
}

func sKey(id string) string   { return "s:" + id }
func cKey(d, c string) string { return "c:" + d + ":" + c }
func oKey(d string) string    { return "o:" + d }
func ttlS() string            { return strconv.Itoa(int(keyTTL.Seconds())) }

func (v *vkState) get(ctx context.Context, id string) (*store.Session, string, string, error) {
	h, err := v.c.HGetAll(ctx, sKey(id)).Result()
	if err != nil || len(h) == 0 {
		return nil, "", "", err
	}
	st := &secc.State{}
	if err := json.Unmarshal([]byte(h["st"]), st); err != nil {
		return nil, "", "", err
	}
	ver, _ := strconv.ParseInt(h["v"], 10, 64)
	if h["ended"] == "1" {
		return nil, h["dev"], h["conn"], errEnded
	}
	return &store.Session{ID: id, State: st, Ver: ver}, h["dev"], h["conn"], nil
}

// seedScript: KEYS = s:<id>, c:<dev>:<conn>, o:<dev>, open; ARGV = device,
// conn, state JSON, ttl s, open (1/0), session id. Writes the session only if
// it is not in Valkey yet; returns 0 when it was.
var seedScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('HSET', KEYS[1], 'dev', ARGV[1], 'conn', ARGV[2], 'st', ARGV[3], 'v', 1)
redis.call('EXPIRE', KEYS[1], ARGV[4])
redis.call('SET', KEYS[2], ARGV[6], 'EX', ARGV[4], 'NX')
if ARGV[5] == '1' then
  redis.call('HSET', KEYS[3], 'sid', ARGV[6], 'conn', ARGV[2])
  redis.call('EXPIRE', KEYS[3], ARGV[4])
  redis.call('SADD', KEYS[4], ARGV[1])
end
return 1`)

// seed puts a new session into Valkey — only if it is not there yet — and returns what Valkey holds.
// An unconditional write lost steps (found 09.11.2026, 3 of 890 sessions with
// two engines): instance B, finding the session in Postgres but not yet in
// Valkey, seeded and stepped it; A's seed of the new session came after and
// reset the state to before the step; the EV's next request then failed to
// decode against the reset state.
func (v *vkState) seed(ctx context.Context, device, conn string, s *store.Session, open bool) (*store.Session, error) {
	raw, _ := json.Marshal(s.State)
	o := "0"
	if open {
		o = "1"
	}
	n, err := seedScript.Run(ctx, v.c, []string{sKey(s.ID), cKey(device, conn), oKey(device), "open"},
		device, conn, raw, ttlS(), o, s.ID).Int()
	if err != nil {
		return nil, err
	}
	if n == 1 {
		s.Ver = 1
		return s, nil
	}
	cur, _, _, err := v.get(ctx, s.ID) // seeded by another instance meanwhile
	return cur, err
}

func (v *vkState) Load(ctx context.Context, device, conn string) (*store.Session, error) {
	id, err := v.c.Get(ctx, cKey(device, conn)).Result()
	if err == nil {
		s, _, _, err := v.get(ctx, id)
		if err != nil || s != nil {
			return s, err
		}
		// Claimed but not written yet: Create waits for it.
		return nil, nil
	} else if !errors.Is(err, redis.Nil) {
		return nil, err
	}
	// Not in Valkey: none, or one lost with Valkey (ADR-002: its state is
	// not kept anywhere else; the EV gets FAILED_UnknownSession and starts over).
	return nil, nil
}

func (v *vkState) Create(ctx context.Context, device, conn string, st *secc.State) (*store.Session, error) {
	id := uuid4()
	// Claim the connection first: the "open" event and the EV's first
	// request can reach two engine instances at the same moment, and only
	// one of them may create the session (found 08.11.2026).
	ok, err := v.c.SetNX(ctx, cKey(device, conn), id, keyTTL).Result()
	if err != nil {
		return nil, err
	}
	if !ok { // the other instance is creating it: wait for it to appear
		for i := 0; i < 50; i++ {
			if other, _ := v.c.Get(ctx, cKey(device, conn)).Result(); other != "" {
				if s, _, _, err := v.get(ctx, other); err != nil || s != nil {
					return s, err
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil, fmt.Errorf("session of %s/%s: created elsewhere, not visible", device, conn)
	}
	// Nothing in Postgres (ADR-002): its sessions row comes from the feed
	// events (projector).
	return v.seed(ctx, device, conn, &store.Session{ID: id, State: st}, true)
}

func (v *vkState) Save(ctx context.Context, s *store.Session, evs ...events.Event) error {
	raw, _ := json.Marshal(s.State)
	dev := ""
	if len(evs) > 0 {
		dev = evs[0].Device
	}
	ends := "0"
	if events.Ends(evs) {
		ends = "1"
	}
	args := []any{s.Ver, raw, ttlS(), v.maxLen, dev, s.ID, ends}
	for _, e := range evs {
		args = append(args, e.Type, evTime(e.Time), evData(e.Payload()))
	}
	nv, err := casSave.Run(ctx, v.c, []string{sKey(s.ID), streamKey, dseqKey(dev)}, args...).Int64()
	if err != nil {
		return err
	}
	if nv < 0 {
		return errConflict
	}
	s.Ver = nv
	return nil
}

func (v *vkState) unopen(ctx context.Context, device, sid string) {
	if cur, _ := v.c.HGet(ctx, oKey(device), "sid").Result(); cur == sid || sid == "" {
		v.c.Del(ctx, oKey(device))
		v.c.SRem(ctx, "open", device)
	}
}

// end: ended flag, new version and (once) v2g.ended — see closeScript.
func (v *vkState) end(ctx context.Context, device, id, reason string) {
	if err := closeScript.Run(ctx, v.c, []string{sKey(id), streamKey, dseqKey(device)},
		v.maxLen, device, id, evTime(time.Time{}), evData(map[string]any{"reason": reason})).Err(); err != nil {
		v.log.Error("end session", "session", id, "err", err)
	}
}

func (v *vkState) Close(ctx context.Context, device, conn, reason string) error {
	if id, err := v.c.Get(ctx, cKey(device, conn)).Result(); err == nil {
		v.end(ctx, device, id, reason)
		v.unopen(ctx, device, id)
	}
	return nil // the sessions row ends with v2g.ended (projector, ADR-002)
}

func (v *vkState) CloseAll(ctx context.Context, device, reason string) (int64, error) {
	var n int64
	if id, err := v.c.HGet(ctx, oKey(device), "sid").Result(); err == nil {
		v.end(ctx, device, id, reason)
		n = 1
	}
	v.unopen(ctx, device, "")
	return n, nil // rows left open in Postgres: closed by the projector on unplug (ADR-002)
}

// Emit: one Lua call per device, all in one pipeline (the script is loaded
// first: inside a pipeline there is no fallback from EVALSHA to EVAL).
func (v *vkState) Emit(ctx context.Context, evs ...events.Event) error {
	if len(evs) == 0 {
		return nil
	}
	by := map[string][]any{}
	for _, e := range evs {
		by[e.Device] = append(by[e.Device], e.SessionID, e.Type, evTime(e.Time), evData(e.Payload()))
	}
	if len(by) == 1 {
		for dev, a := range by {
			return emitScript.Run(ctx, v.c, []string{streamKey, dseqKey(dev)}, append([]any{v.maxLen, dev}, a...)...).Err()
		}
	}
	if err := emitScript.Load(ctx, v.c).Err(); err != nil {
		return err
	}
	p := v.c.Pipeline()
	for dev, a := range by {
		p.EvalSha(ctx, emitScript.Hash(), []string{streamKey, dseqKey(dev)}, append([]any{v.maxLen, dev}, a...)...)
	}
	_, err := p.Exec(ctx)
	if err == redis.Nil {
		err = nil
	}
	return err
}

func (v *vkState) Power(ctx context.Context, device, sid string, ord order, at time.Time, plug string, w, eWh float64) (bool, error) {
	ev := powerEvent(device, sid, at, plug, w)
	n, err := powerScript.Run(ctx, v.c, []string{streamKey, dseqKey(device), pwKey(device)},
		v.maxLen, device, sid, ord.seq, math.Round(w), powerStep, evTime(ev.Time), evData(ev.Payload()), eWh, ord.boot).Int()
	return n == 1, err
}

// openScript: KEYS o:<dev>; the device's open session in one call:
// {sid, conn, st, v, ended} or nil when the device has none in Valkey.
var openScript = redis.NewScript(`
local sid = redis.call('HGET', KEYS[1], 'sid')
if not sid then return nil end
local h = redis.call('HMGET', 's:' .. sid, 'conn', 'st', 'v', 'ended')
if not h[2] then return {sid} end
return {sid, h[1] or '', h[2], h[3] or '0', h[4] or '0'}`)

func (v *vkState) Open(ctx context.Context, device string) ([]store.OpenSession, error) {
	if device != "" { // one round trip (an activation send needs it on its fast path)
		r, err := openScript.Run(ctx, v.c, []string{oKey(device)}).StringSlice()
		if err == nil && len(r) == 5 {
			if r[4] == "1" {
				return nil, nil // ended meanwhile
			}
			st := &secc.State{}
			if err := json.Unmarshal([]byte(r[2]), st); err != nil {
				return nil, err
			}
			ver, _ := strconv.ParseInt(r[3], 10, 64)
			return []store.OpenSession{{Device: device, Conn: r[1], Session: store.Session{ID: r[0], State: st, Ver: ver}}}, nil
		}
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		// none in Valkey (or a stale pointer): the slow path below
	}
	devices := []string{device}
	if device == "" {
		var err error
		if devices, err = v.c.SMembers(ctx, "open").Result(); err != nil {
			return nil, err
		}
	}
	var out []store.OpenSession
	for _, d := range devices {
		h, err := v.c.HGetAll(ctx, oKey(d)).Result()
		if err != nil {
			return nil, err
		}
		if len(h) == 0 { // none, or lost with Valkey (ADR-002: no copy in Postgres)
			if device == "" {
				v.c.SRem(ctx, "open", d)
			}
			continue
		}
		s, _, conn, err := v.get(ctx, h["sid"])
		if err != nil || s == nil {
			continue
		}
		out = append(out, store.OpenSession{Device: d, Conn: conn, Session: *s})
	}
	return out, nil
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
