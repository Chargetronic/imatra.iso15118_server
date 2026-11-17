// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Hot station state (activations rework, 10.11.2026). With Valkey every
// instance reads one shared record per device instead of its own copy loaded
// from Postgres:
//
//	st:<device>  hash  cfg   station settings (JSON, without the limit)
//	                   cmd, lim, prio, until (unix ms), src: the limit in force
//
// Written once when a device is first needed (from Postgres), then kept up
// to date by whoever issues a command: an activation stage writes its limits
// here before the sends (hotLimit: only where it wins on priority, then
// command id), so a send reads nothing from Postgres. A direct limit or a new
// configuration (NOTIFY) and the end of an activation remove the record; the
// next read rebuilds it from Postgres. Without a Valkey client (tests) the same
// lives in the engine's memory (caches.go).

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

const hotTTL = time.Hour // settings are re-read from Postgres at least hourly

func hotKey(d string) string { return "st:" + d }

// hotLimit: KEYS st:<dev>; ARGV cmd, limit A, priority, until ms, source.
// Writes the limit if the record exists and the new limit wins over the one
// stored (higher priority, or same priority and newer command, or the stored
// one expired). 1 written, 0 no record, -1 a stronger limit holds.
var hotLimit = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cur = redis.call('HMGET', KEYS[1], 'cmd', 'prio', 'until')
local cc, cp, cu = tonumber(cur[1] or '0'), tonumber(cur[2] or '0'), tonumber(cur[3] or '0')
local nc, np = tonumber(ARGV[1]), tonumber(ARGV[3])
if cc ~= 0 and cu > now and (cp > np or (cp == np and cc >= nc)) then return -1 end
redis.call('HSET', KEYS[1], 'cmd', ARGV[1], 'lim', ARGV[2], 'prio', ARGV[3], 'until', ARGV[4], 'src', ARGV[5])
return 1`)

func (e *engine) vk() *redis.Client {
	if v, ok := e.st.(*vkState); ok {
		return v.c
	}
	return nil
}

// hotGet: the station settings with the limit in force, from Valkey.
func (e *engine) hotGet(ctx context.Context, c *redis.Client, device string) (secc.Config, bool) {
	h, err := c.HGetAll(ctx, hotKey(device)).Result()
	if err != nil || h["cfg"] == "" {
		return secc.Config{}, false
	}
	var cfg secc.Config
	if json.Unmarshal([]byte(h["cfg"]), &cfg) != nil {
		return secc.Config{}, false
	}
	cfg.LimitA, cfg.CommandID = 0, 0
	until, _ := strconv.ParseInt(h["until"], 10, 64)
	if cmd, _ := strconv.ParseInt(h["cmd"], 10, 64); cmd != 0 && until > time.Now().UnixMilli() {
		cfg.CommandID = cmd
		cfg.LimitA, _ = strconv.ParseFloat(h["lim"], 64)
		cfg.LimitPrio, _ = strconv.Atoi(h["prio"])
		cfg.LimitSource = h["src"]
		cfg.LimitUntil = time.UnixMilli(until)
	}
	return cfg, true
}

// hotPut: the record as read from Postgres.
func (e *engine) hotPut(ctx context.Context, c *redis.Client, device string, cfg secc.Config) {
	base := cfg
	base.LimitA = 0
	raw, _ := json.Marshal(base)
	until := int64(0)
	if cfg.CommandID != 0 {
		until = cfg.LimitUntil.UnixMilli()
	}
	p := c.TxPipeline()
	p.Del(ctx, hotKey(device))
	p.HSet(ctx, hotKey(device), "cfg", raw, "cmd", cfg.CommandID, "lim", cfg.LimitA, "prio", cfg.LimitPrio,
		"until", until, "src", cfg.LimitSource)
	p.Expire(ctx, hotKey(device), hotTTL)
	if _, err := p.Exec(ctx); err != nil {
		e.log.Warn("hot station state", "device", device, "err", err)
	}
}

// hotLimits: an activation stage's limits into the records, one pipeline.
// The script is loaded first: inside a pipeline go-redis cannot fall back
// from EVALSHA to EVAL (found 10.11.2026: NOSCRIPT, no limit written, the
// sends went out with the old one). If the write fails anyway, the records
// are dropped, so the sends read the limit from Postgres: slower, never wrong.
func (e *engine) hotLimits(ctx context.Context, c *redis.Client, devices []string, cmds []int64, limitA float64, prio int, until time.Time) {
	err := hotLimit.Load(ctx, c).Err()
	if err == nil {
		p := c.Pipeline()
		for i, d := range devices {
			p.EvalSha(ctx, hotLimit.Hash(), []string{hotKey(d)}, cmds[i], limitA, prio, until.UnixMilli(), "activation")
		}
		if _, err = p.Exec(ctx); err == redis.Nil {
			err = nil
		}
	}
	if err != nil {
		e.log.Error("hot limits, falling back to Postgres for these sends", "devices", len(devices), "err", err)
		e.hotDrop(ctx, c, devices...)
	}
}

// hotDrop: the records are rebuilt from Postgres on the next read.
func (e *engine) hotDrop(ctx context.Context, c *redis.Client, devices ...string) {
	if len(devices) == 0 {
		return
	}
	keys := make([]string, len(devices))
	for i, d := range devices {
		keys[i] = hotKey(d)
	}
	for i := 0; i < len(keys); i += 1000 {
		j := min(i+1000, len(keys))
		if err := c.Del(ctx, keys[i:j]...).Err(); err != nil {
			e.log.Warn("hot state drop", "err", err)
		}
	}
}

// hotOpen: KEYS st:<dev>, o:<dev>; the station record and the open session in
// one call: {cfg, cmd, lim, prio, until, src, sid, conn, st, v, ended}
// (missing parts empty).
var hotOpen = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'cfg', 'cmd', 'lim', 'prio', 'until', 'src')
local out = {h[1] or '', h[2] or '0', h[3] or '0', h[4] or '0', h[5] or '0', h[6] or ''}
local sid = redis.call('HGET', KEYS[2], 'sid')
if sid then
  local s = redis.call('HMGET', 's:' .. sid, 'conn', 'st', 'v', 'ended')
  if s[2] then
    out[7], out[8], out[9], out[10], out[11] = sid, s[1] or '', s[2], s[3] or '0', s[4] or '0'
  end
end
return out`)

// configAndOpen: the station settings, and for an activation send also the
// open session (ok: it was read; open nil: none). Falls back to the usual
// reads where the hot record is missing.
func (e *engine) configAndOpen(ctx context.Context, device, why string) (secc.Config, []store.OpenSession, bool) {
	vk := e.vk()
	if vk == nil || (why != "activation" && why != whyResend) {
		return e.stationConfig(ctx, device), nil, false
	}
	r, err := hotOpen.Run(ctx, vk, []string{hotKey(device), oKey(device)}).StringSlice()
	if err != nil || len(r) < 6 || r[0] == "" {
		return e.stationConfig(ctx, device), nil, false
	}
	var cfg secc.Config
	if json.Unmarshal([]byte(r[0]), &cfg) != nil {
		return e.stationConfig(ctx, device), nil, false
	}
	cfg.LimitA, cfg.CommandID = 0, 0
	until, _ := strconv.ParseInt(r[4], 10, 64)
	if cmd, _ := strconv.ParseInt(r[1], 10, 64); cmd != 0 && until > time.Now().UnixMilli() {
		cfg.CommandID = cmd
		cfg.LimitA, _ = strconv.ParseFloat(r[2], 64)
		cfg.LimitPrio, _ = strconv.Atoi(r[3])
		cfg.LimitSource = r[5]
		cfg.LimitUntil = time.UnixMilli(until)
		e.watchExpiry(device, cfg.LimitUntil)
	}
	if len(r) < 11 {
		return cfg, nil, false // no open session in Valkey: the usual read decides
	}
	if r[10] == "1" {
		return cfg, nil, true // ended
	}
	st := &secc.State{}
	if json.Unmarshal([]byte(r[8]), st) != nil {
		return cfg, nil, false
	}
	ver, _ := strconv.ParseInt(r[9], 10, 64)
	return cfg, []store.OpenSession{{Device: device, Conn: r[7], Session: store.Session{ID: r[6], State: st, Ver: ver}}}, true
}
