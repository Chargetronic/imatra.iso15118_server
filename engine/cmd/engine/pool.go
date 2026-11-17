// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

// Pool of devices for activations (10.11.2026). The devices an activation
// can take are known before it is called: every feed event of a device also
// updates its hot record and its place in the pool, in the same Lua call
// that writes the event (luaEmit), so a stage start reads nothing from
// Postgres to choose its devices.
//
//	hs:<device>  hash  plug   plug-in start (unix s); no plug: not plugged in
//	                   on     1 online / 0 offline
//	                   mode   iso | din | pwm (how this plug-in charges)
//	                   chg    1 charging / 0 not
//	                   pw     power now, W
//	                   dep    departure (unix s), when the EV told it
//	                   need   energy the EV still asked for, Wh, at need_at (unix s)
//	                   evmax  EV maximum power, W
//	                   rx     reaction to a limit, ms (smoothed over the commands)
//	                   eplug  energy of this plug-in, Wh; elast the device's last counter
//	                          (powerScript, statestore.go)
//	                   e2g    eplug when the digital session started (its energy at v2g.ended)
//	                   lev    the last limit.changed in the feed (only a change is emitted)
//	                   seeded the record came from Postgres (Valkey started empty): its
//	                          energy misses what came before; the unplug reads Postgres
//	                   lat, lon  coarse location of the device (registry; from Postgres at plug-in)
//	                   out    1: unbound or revoked (registry): never in the pool
//	pool         zset  online plugged-in devices; score: mode (ISO 2e9, DIN 1e9,
//	                   PWM 0) + charging 5e8 + power W: the best first
//	act:<id>     set   devices an activation took already
//
// A stage start (poolPick) walks the pool from the top and takes the devices
// that pass (thresholds: operator settings, env):
//   - the selection's mode and charging, if given;
//   - departure later than the activation's end + ACT_DEPART_MARGIN_S (300 s:
//     the EV must not leave during the limit; unknown departure passes);
//   - energy still asked for larger than what the EV gets at its power now
//     until the activation ends (unknown — PWM — passes, ranked lower);
//   - reaction to limits under ACT_MAX_REACTION_MS (5000 ms; unknown passes);
//   - no limit of higher priority in force (st:<device>), not taken already.
// Rx, elast, lat and lon outlive a plug-in; the rest is cleared at plug-in and unplug.
// The keys are named in the scripts, not passed: Valkey standalone (no cluster).

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

// luaHot: hot(dev, typ, data), called by emit for every feed event.
var luaHot = `
local function repool(dev, k)
  local h = redis.call('HMGET', k, 'plug', 'on', 'mode', 'chg', 'pw', 'out')
  if not h[1] or h[2] == '0' or h[6] == '1' then redis.call('ZREM', 'pool', dev) return end
  local cls = 0
  if h[3] == 'iso' then cls = 2e9 elseif h[3] == 'din' then cls = 1e9 end
  local s = cls + (h[4] == '1' and 5e8 or 0) + math.min(tonumber(h[5] or '0') or 0, 4e8)
  redis.call('ZADD', 'pool', s, dev)
end
local plugFields = {'plug', 'mode', 'chg', 'pw', 'dep', 'need', 'need_at', 'evmax', 'eplug', 'seeded', 'e2g'}
-- limit.changed only when the limit in force changed (current, source,
-- command, end): a limit sent again (resend, reconnect, refresh) is not news.
-- The last one is kept in the device's record (lev), outliving plug-ins.
local function limitSame(dev, data)
  local ok, d = pcall(cjson.decode, data)
  if not ok or type(d) ~= 'table' then return false end
  local key = tostring(d.limit_a) .. '|' .. tostring(d.source) .. '|' .. tostring(d.command_id) .. '|' .. tostring(d['until'])
  local hk = 'hs:' .. dev
  if redis.call('HGET', hk, 'lev') == key then return true end
  redis.call('HSET', hk, 'lev', key)
  redis.call('EXPIRE', hk, 172800)
  return false
end
-- v2g.ended carries the energy of the digital session (plug-in energy now
-- minus at its start), so nothing has to scan the telemetry for it.
local function v2gEnergy(dev, data)
  local h = redis.call('HMGET', 'hs:' .. dev, 'e2g', 'eplug', 'seeded')
  if not h[1] or h[3] then return data end
  local ok, d = pcall(cjson.decode, data)
  if not ok or type(d) ~= 'table' then return data end
  d.energy_wh = math.floor((tonumber(h[2] or '0') - tonumber(h[1])) * 100 + 0.5) / 100
  return cjson.encode(d)
end
local function hot(dev, typ, data)
  local k = 'hs:' .. dev
  local now = tonumber(redis.call('TIME')[1])
  if typ == 'session.ended' then
    redis.call('HDEL', k, unpack(plugFields))
    redis.call('ZREM', 'pool', dev)
    return
  end
  if typ == 'device.unbound' or typ == 'device.revoked' then
    -- not this operator's (any more): never in the pool; a running session goes on
    redis.call('HSET', k, 'out', 1)
  elseif typ == 'device.bound' then
    local d = cjson.decode(data)
    redis.call('HDEL', k, 'out')
    if d.lat and d.lon then redis.call('HSET', k, 'lat', d.lat, 'lon', d.lon) end
  elseif typ == 'device.online' or typ == 'device.offline' then
    redis.call('HSET', k, 'on', typ == 'device.online' and 1 or 0)
  elseif typ == 'session.started' then
    local d = cjson.decode(data)
    local early = redis.call('HMGET', k, 'plug', 'mode')
    if early[1] then redis.call('HDEL', k, unpack(plugFields)) early[2] = nil end
    redis.call('HSET', k, 'plug', now, 'on', 1, 'mode', early[2] or 'pwm', 'chg', d.cp == 'C' and 1 or 0)
  elseif typ == 'v2g.started' then
    local d = cjson.decode(data)
    redis.call('HSET', k, 'mode', d.protocol == 'din70121' and 'din' or 'iso', 'e2g', redis.call('HGET', k, 'eplug') or 0)
  elseif typ == 'charging.started' then redis.call('HSET', k, 'chg', 1)
  elseif typ == 'charging.stopped' then redis.call('HSET', k, 'chg', 0)
  elseif typ == 'cp.changed' then
    local d = cjson.decode(data)
    if d.mode == 'basic' or d.mode == 'transit' or redis.call('HGET', k, 'mode') == 'pwm' then
      redis.call('HSET', k, 'chg', d.ev == 'C' and 1 or 0)
    end
  elseif typ == 'power.changed' then
    local d = cjson.decode(data)
    if d.power_w then redis.call('HSET', k, 'pw', d.power_w) end
  elseif typ == 'ev.parameters' then
    local d = cjson.decode(data)
    if d.e_amount_wh then redis.call('HSET', k, 'need', d.e_amount_wh, 'need_at', now) end
    if d.departure_time_s then redis.call('HSET', k, 'dep', now + d.departure_time_s) end
    if d.ev_max_power_w then redis.call('HSET', k, 'evmax', d.ev_max_power_w) end
  elseif typ == 'limit.applied' then
    local d = cjson.decode(data)
    if d.reaction_ms then
      local o = tonumber(redis.call('HGET', k, 'rx') or '')
      redis.call('HSET', k, 'rx', o and math.floor(0.7 * o + 0.3 * d.reaction_ms) or d.reaction_ms)
    end
    return
  else
    return
  end
  redis.call('EXPIRE', k, 172800)
  repool(dev, k)
end
`

// poolPick: ARGV activation, priority, activation end (unix s), margin s,
// max reaction ms, charging (” | '1' | '0'), mode (” | iso | din | pwm),
// max devices (0: all). Returns {device, power W, ...} best first.
var poolPick = redis.NewScript(`
local t = redis.call('TIME')
local now, nowms = tonumber(t[1]), tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local aid, prio, ends, margin, maxrx = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5])
local wchg, wmode, max = ARGV[6], ARGV[7], tonumber(ARGV[8])
local left_s = math.max(0, ends - now)
local out, n = {}, 0
for _, d in ipairs(redis.call('ZREVRANGE', 'pool', 0, -1)) do
  local h = redis.call('HMGET', 'hs:' .. d, 'mode', 'chg', 'pw', 'dep', 'need', 'need_at', 'rx')
  local pw = tonumber(h[3] or '0') or 0
  local ok = (wchg == '' or h[2] == wchg) and (wmode == '' or h[1] == wmode)
  if ok and h[4] and tonumber(h[4]) < ends + margin then ok = false end
  if ok and h[5] then
    local need = tonumber(h[5]) - pw * (now - tonumber(h[6] or now)) / 3600
    if need < pw * left_s / 3600 then ok = false end
  end
  if ok and h[7] and tonumber(h[7]) > maxrx then ok = false end
  if ok then
    local s = redis.call('HMGET', 'st:' .. d, 'cmd', 'prio', 'until')
    if s[1] and s[1] ~= '0' and tonumber(s[3] or '0') > nowms and tonumber(s[2] or '0') > prio then ok = false end
  end
  if ok and redis.call('SISMEMBER', 'act:' .. aid, d) == 1 then ok = false end
  if ok then
    n = n + 1
    out[#out + 1] = d
    out[#out + 1] = tostring(pw)
    if max > 0 and n >= max then break end
  end
end
return out`)

// poolPicker: the stage start's choice from the pool (store.StartDueStage).
func (e *engine) poolPicker(vk *redis.Client) store.Picker {
	margin := envF("ACT_DEPART_MARGIN_S", 300)
	maxRx := envF("ACT_MAX_REACTION_MS", 5000)
	return func(ctx context.Context, p store.Pick) ([]store.Candidate, error) {
		chg, mode := "", ""
		if p.Charging != nil {
			chg = map[bool]string{true: "1", false: "0"}[*p.Charging]
		}
		if p.Mode != nil {
			mode = *p.Mode
		}
		r, err := poolPick.Run(ctx, vk, nil, p.Activation, p.Priority, p.Ends.Unix(), margin, maxRx, chg, mode, p.Max).StringSlice()
		if err != nil {
			return nil, err
		}
		out := make([]store.Candidate, 0, len(r)/2)
		for i := 0; i+1 < len(r); i += 2 {
			w, _ := strconv.ParseFloat(r[i+1], 64)
			out = append(out, store.Candidate{Device: r[i], PowerW: w})
		}
		return out, nil
	}
}

// poolTaken: the devices of a started stage, so the next stages skip them.
func poolTaken(ctx context.Context, vk *redis.Client, activation string, devs []string, until time.Time) {
	if len(devs) == 0 {
		return
	}
	k := "act:" + activation
	m := make([]any, len(devs))
	for i, d := range devs {
		m[i] = d
	}
	p := vk.Pipeline()
	p.SAdd(ctx, k, m...)
	p.ExpireAt(ctx, k, until.Add(time.Hour))
	_, _ = p.Exec(ctx)
}

// hsSeed: KEYS hs:<dev>; ARGV device, plug unix s, online, mode, charging, power W.
// Fills the record from Postgres where the events did not (Valkey started
// empty with devices plugged in); a record with a plug-in is left alone.
var hsSeed = redis.NewScript(luaHot + `
if redis.call('HEXISTS', KEYS[1], 'plug') == 1 then return 0 end
redis.call('HSET', KEYS[1], 'plug', ARGV[2], 'on', ARGV[3], 'mode', ARGV[4], 'chg', ARGV[5], 'pw', ARGV[6], 'seeded', 1)
redis.call('EXPIRE', KEYS[1], 172800)
repool(ARGV[1], KEYS[1])
return 1`)

// poolSeed (leader): once per Valkey life (marker pool:seeded), the devices
// plugged in now from the operator tables, and every device's location.
func (e *engine) poolSeed(ctx context.Context, vk *redis.Client) {
	if ok, err := vk.SetNX(ctx, "pool:seeded", time.Now().Unix(), 0).Result(); err != nil || !ok {
		return
	}
	rows, err := e.db.PoolSeed(ctx)
	if err != nil {
		e.log.Error("pool seed", "err", err)
		vk.Del(ctx, "pool:seeded")
		return
	}
	if err := hsSeed.Load(ctx, vk).Err(); err != nil {
		vk.Del(ctx, "pool:seeded")
		return
	}
	p := vk.Pipeline()
	n := 0
	for _, r := range rows {
		k := "hs:" + r.Device
		if r.Lat != nil {
			p.HSet(ctx, k, "lat", *r.Lat, "lon", *r.Lon)
		}
		if r.Plugged != nil {
			n++
			p.EvalSha(ctx, hsSeed.Hash(), []string{k}, r.Device, r.Plugged.Unix(), b01(r.Online), r.Mode, b01(r.Charging), r.PowerW)
		}
	}
	if _, err := p.Exec(ctx); err != nil && err != redis.Nil {
		e.log.Error("pool seed", "err", err)
	}
	e.log.Info("activation pool seeded", "devices", len(rows), "plugged", n)
}

// hsLocation: a device plugged in: its coarse location into the hot record.
func (e *engine) hsLocation(device string) {
	vk := e.vk()
	if vk == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lat, lon, err := e.db.Location(ctx, device)
	if err != nil || lat == nil {
		return
	}
	vk.HSet(ctx, "hs:"+device, "lat", *lat, "lon", *lon)
}

// plugEnergy: energy and duration of the plug-in ending now, from the hot
// record; false when there is none (no Valkey, or the record was lost).
func (e *engine) plugEnergy(ctx context.Context, device string) (wh float64, seconds int, ok bool) {
	vk := e.vk()
	if vk == nil {
		return 0, 0, false
	}
	r, err := vk.HMGet(ctx, "hs:"+device, "plug", "eplug", "seeded").Result()
	if err != nil || r[0] == nil || r[2] != nil { // seeded: the energy before it is not in the record
		return 0, 0, false
	}
	plug, _ := strconv.ParseInt(r[0].(string), 10, 64)
	if r[1] != nil {
		wh, _ = strconv.ParseFloat(r[1].(string), 64)
	}
	return wh, int(time.Now().Unix() - plug), true
}

func b01(b bool) int {
	if b {
		return 1
	}
	return 0
}
