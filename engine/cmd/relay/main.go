// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Relay: the operator's event stream in Valkey -> domain_events in Postgres.
//
//   - At least once: a consumer group; XACK only after the batch is committed.
//     Entries of a dead relay are taken over after RELAY_CLAIM_S (XAUTOCLAIM).
//   - Idempotent: the stream entry id is the event id; a repeated insert is
//     skipped (ON CONFLICT (id) DO NOTHING). seq is given by Postgres.
//   - Retention: stored entries are trimmed (XTRIM MINID at the oldest entry
//     not stored yet); hard limits EVENTS_STREAM_MAX entries and
//     EVENTS_STREAM_HOURS (48 h), enforced here. The engines only cap the
//     stream at 1.25 x EVENTS_STREAM_MAX, to protect Valkey while no relay
//     runs.
//   - Gaps: entries lost before they were stored (hard limit reached, Valkey
//     restarted and lost its tail) are reported as system.events_gap with the
//     interval, so consumers know to re-read that period through the API.
//   - Several relays may run in one group; one is the default.
//
// Valkey without AOF is refused at start (RELAY_ALLOW_NO_AOF=true for tests).
package main

import (
	"sync/atomic"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/metrics"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
)

const (
	group = "relay"
	name  = "events" // relay_state row
)

type relay struct {
	log    *slog.Logger
	c      *redis.Client
	db     *store.Store
	stream string
	me     string
	batch  int64
	max    int64 // EVENTS_STREAM_MAX
	claim  time.Duration
	keep   time.Duration

	stored  atomic.Int64 // metrics
	dupes   atomic.Int64
	lastID  atomic.Value // string: newest entry stored by this relay
	healthy atomic.Bool
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envI(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		r, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://127.0.0.1:8091/health")
		if err != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable connect_timeout=5",
		env("POSTGRES_HOST", "postgres"), env("POSTGRES_PORT", "5432"), os.Getenv("POSTGRES_DB"),
		os.Getenv("POSTGRES_USER"), os.Getenv("POSTGRES_PASSWORD"))
	os.Setenv("DB_MAX_CONNS", env("RELAY_DB_CONNS", "4"))
	var db *store.Store
	var err error
	for i := 0; ; i++ {
		if db, err = store.Open(dsn); err == nil {
			break
		}
		if i == 30 {
			log.Error("database not reachable", "err", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
	_ = db.WaitSchema(context.Background(), log)
	host, _ := os.Hostname()
	r := &relay{
		log:    log,
		c:      redis.NewClient(&redis.Options{Addr: env("VALKEY_ADDR", "valkey:6379"), Password: os.Getenv("VALKEY_PASSWORD")}),
		db:     db,
		stream: env("EVENTS_STREAM", "events"),
		me:     host + "-" + strconv.Itoa(os.Getpid()),
		batch:  int64(envI("RELAY_BATCH", 1000)),
		max:    int64(envI("EVENTS_STREAM_MAX", 500_000)),
		claim:  time.Duration(envI("RELAY_CLAIM_S", 30)) * time.Second,
		keep:   time.Duration(envI("EVENTS_STREAM_HOURS", 48)) * time.Hour,
	}
	r.lastID.Store("")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := r.start(ctx); err != nil {
		log.Error("relay start", "err", err)
		os.Exit(1)
	}
	go r.serveHealth()
	go r.housekeeping(ctx)
	r.run(ctx)
}

func (r *relay) start(ctx context.Context) error {
	for i := 0; ; i++ {
		err := r.c.Ping(ctx).Err()
		if err == nil {
			break
		}
		if i == 30 {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	if aof, err := r.c.ConfigGet(ctx, "appendonly").Result(); err == nil && aof["appendonly"] != "yes" {
		if env("RELAY_ALLOW_NO_AOF", "") != "true" {
			return errors.New("valkey runs without AOF (appendonly no): a restart would lose the event stream; " +
				"start it with --appendonly yes (or RELAY_ALLOW_NO_AOF=true for tests)")
		}
		r.log.Warn("valkey without AOF: events not yet stored are lost on a Valkey restart")
	}
	r.ensureGroup(ctx)
	r.checkGaps(ctx)
	r.log.Info("relay started", "stream", r.stream, "consumer", r.me, "batch", r.batch)
	return nil
}

// ensureGroup creates the consumer group (at the start of the stream: a new
// relay stores everything still in it; duplicates are skipped).
func (r *relay) ensureGroup(ctx context.Context) {
	err := r.c.XGroupCreateMkStream(ctx, r.stream, group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		r.log.Error("consumer group", "err", err)
	}
}

func (r *relay) run(ctx context.Context) {
	nextClaim := time.Now()
	for ctx.Err() == nil {
		if time.Now().After(nextClaim) {
			r.claimStale(ctx)
			nextClaim = time.Now().Add(r.claim / 3)
		}
		res, err := r.c.XReadGroup(ctx, &redis.XReadGroupArgs{Group: group, Consumer: r.me,
			Streams: []string{r.stream, ">"}, Count: r.batch, Block: time.Second}).Result()
		if errors.Is(err, redis.Nil) {
			r.healthy.Store(true)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if strings.HasPrefix(err.Error(), "NOGROUP") { // Valkey lost the stream
				r.log.Warn("consumer group gone, recreating", "err", err)
				r.ensureGroup(ctx)
				r.checkGaps(ctx)
				continue
			}
			r.healthy.Store(false)
			r.log.Error("read stream", "err", err)
			time.Sleep(time.Second)
			continue
		}
		for _, s := range res {
			r.store(ctx, s.Messages)
		}
	}
}

// claimStale takes over entries a dead relay read but never stored.
func (r *relay) claimStale(ctx context.Context) {
	start := "0-0"
	for ctx.Err() == nil {
		msgs, next, err := r.c.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: r.stream, Group: group,
			Consumer: r.me, MinIdle: r.claim, Start: start, Count: r.batch}).Result()
		if err != nil {
			return
		}
		if len(msgs) > 0 {
			r.log.Info("taken over from another relay", "entries", len(msgs))
			r.store(ctx, msgs)
		}
		if next == "0-0" || len(msgs) == 0 {
			return
		}
		start = next
	}
}

// store commits a batch, then acknowledges it; on failure it retries until
// Postgres is back (nothing is acknowledged before that).
func (r *relay) store(ctx context.Context, msgs []redis.XMessage) {
	if len(msgs) == 0 {
		return
	}
	rows := make([]store.DomainRow, 0, len(msgs))
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
		if row, ok := toRow(m); ok {
			rows = append(rows, row)
		} else {
			r.log.Warn("malformed stream entry, skipped", "id", m.ID)
		}
	}
	for wait := time.Second; ctx.Err() == nil; wait = min(wait*2, 30*time.Second) {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		n, err := r.db.InsertDomainEvents(c, rows)
		cancel()
		if err == nil {
			r.stored.Add(n)
			r.dupes.Add(int64(len(rows)) - n)
			// Stored: before the ack, so a trim right after it is never
			// mistaken for a gap.
			if idLess(r.lastID.Load().(string), ids[len(ids)-1]) {
				r.lastID.Store(ids[len(ids)-1])
			}
			break
		}
		r.healthy.Store(false)
		r.log.Error("store events, retrying", "entries", len(rows), "in", wait, "err", err)
		time.Sleep(wait)
	}
	if ctx.Err() != nil {
		return // not acknowledged: the next relay (or this one restarted) stores them again
	}
	if err := r.c.XAck(ctx, r.stream, group, ids...).Err(); err != nil {
		r.log.Error("ack", "err", err) // stored; a repeat is skipped as a duplicate
	}
	r.healthy.Store(true)
}

func toRow(m redis.XMessage) (store.DomainRow, bool) {
	str := func(k string) string { s, _ := m.Values[k].(string); return s }
	ts, err := time.Parse(time.RFC3339Nano, str("ts"))
	if err != nil || str("t") == "" {
		return store.DomainRow{}, false
	}
	ver, _ := strconv.Atoi(str("v"))
	q, _ := strconv.ParseInt(str("q"), 10, 64)
	data := json.RawMessage(str("j"))
	if !json.Valid(data) {
		data = json.RawMessage("{}")
	}
	return store.DomainRow{ID: m.ID, Type: str("t"), SchemaVer: ver, Device: str("d"), SessionID: str("s"),
		DSeq: q, Time: ts, Data: data}, true
}

// housekeeping: progress and lag for the API, trimming, gap detection.
func (r *relay) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for i := 0; ctx.Err() == nil; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		lag, backlog := r.lag(ctx)
		st := store.RelayState{LastID: r.lastID.Load().(string)}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := r.db.SaveRelayState(c, name, st, lag, backlog); err != nil {
			r.log.Warn("relay state", "err", err)
		}
		cancel()
		if i%5 == 0 {
			r.trim(ctx)
			r.checkGaps(ctx)
		}
		if i%60 == 0 {
			r.log.Info("relay", "stored", r.stored.Load(), "duplicates", r.dupes.Load(), "lag_ms", lag, "backlog", backlog)
		}
	}
}

// lag: age of the oldest entry not stored yet (pending or not read), ms.
func (r *relay) lag(ctx context.Context) (int64, int64) {
	oldest := ""
	var backlog int64
	if p, err := r.c.XPending(ctx, r.stream, group).Result(); err == nil && p.Count > 0 {
		oldest, backlog = p.Lower, p.Count
	}
	if gs, err := r.c.XInfoGroups(ctx, r.stream).Result(); err == nil {
		for _, g := range gs {
			if g.Name != group || g.Lag <= 0 {
				continue
			}
			backlog += g.Lag
			if oldest == "" {
				if xs, err := r.c.XRangeN(ctx, r.stream, "("+g.LastDeliveredID, "+", 1).Result(); err == nil && len(xs) > 0 {
					oldest = xs[0].ID
				}
			}
		}
	}
	if oldest == "" {
		return 0, backlog
	}
	return max(0, time.Now().UnixMilli()-idMs(oldest)), backlog
}

// trim drops what is stored: everything older than the oldest entry not
// stored yet. Then the hard limits — EVENTS_STREAM_HOURS and
// EVENTS_STREAM_MAX entries — which may cut entries not stored yet: those are
// reported as a gap first. (Valkey's max-deleted-entry-id does not move on a
// trim, so the relay, which knows what it has not stored, does the cutting.)
func (r *relay) trim(ctx context.Context) {
	unstored := r.oldestUnstored(ctx) // "" : everything is stored
	bound := unstored
	if bound == "" {
		if gs, err := r.c.XInfoGroups(ctx, r.stream).Result(); err == nil {
			for _, g := range gs {
				if g.Name == group {
					bound = g.LastDeliveredID
				}
			}
		}
	}
	cut := strconv.FormatInt(time.Now().Add(-r.keep).UnixMilli(), 10) + "-0"
	if n, err := r.c.XLen(ctx, r.stream).Result(); err == nil && n > r.max {
		if xs, err := r.c.XRangeN(ctx, r.stream, "-", "+", n-r.max).Result(); err == nil && len(xs) > 0 {
			if last := xs[len(xs)-1].ID; idLess(cut, last) {
				ms, q := idParts(last)
				cut = strconv.FormatInt(ms, 10) + "-" + strconv.FormatInt(q+1, 10)
			}
		}
	}
	if unstored != "" && idLess(unstored, cut) {
		r.gap(ctx, unstored, cut, "stream limit reached before the entries were stored")
		bound = cut
	} else if bound == "" || idLess(bound, cut) {
		bound = cut
	}
	if err := r.c.XTrimMinID(ctx, r.stream, bound).Err(); err != nil {
		r.log.Warn("trim", "err", err)
	}
}

// oldestUnstored: the oldest pending entry, else the first one not read yet.
func (r *relay) oldestUnstored(ctx context.Context) string {
	if p, err := r.c.XPending(ctx, r.stream, group).Result(); err == nil && p.Count > 0 {
		return p.Lower
	}
	if gs, err := r.c.XInfoGroups(ctx, r.stream).Result(); err == nil {
		for _, g := range gs {
			if g.Name == group {
				if xs, err := r.c.XRangeN(ctx, r.stream, "("+g.LastDeliveredID, "+", 1).Result(); err == nil && len(xs) > 0 {
					return xs[0].ID
				}
			}
		}
	}
	return ""
}

// checkGaps: entries deleted before they were stored (max-deleted-entry-id
// beyond what is stored) or a Valkey restart (its run id changed).
func (r *relay) checkGaps(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := r.db.RelayState(c, name)
	if err != nil {
		r.log.Warn("relay state", "err", err)
		return
	}
	info, err := r.c.XInfoStream(c, r.stream).Result()
	if err != nil && !strings.Contains(err.Error(), "no such key") {
		return
	} // no stream at all (Valkey wiped): the run id still tells
	runID, upS := "", int64(0)
	if s, err := r.c.Info(c, "server").Result(); err == nil {
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if v, ok := strings.CutPrefix(l, "run_id:"); ok {
				runID = v
			} else if v, ok := strings.CutPrefix(l, "uptime_in_seconds:"); ok {
				upS, _ = strconv.ParseInt(v, 10, 64)
			}
		}
	}
	stored := st.LastID
	if l := r.lastID.Load().(string); idLess(stored, l) {
		stored = l
	}
	upd := store.RelayState{RunID: runID}
	// 1) the engines' emergency cap (EVENTS_STREAM_MAX x 1.25) cut entries
	// while no relay was running: the stream is at the cap and starts after
	// what the group has read.
	if gs, err := r.c.XInfoGroups(c, r.stream).Result(); err == nil && info != nil {
		for _, g := range gs {
			if g.Name == group && g.LastDeliveredID != "0-0" && idLess(g.LastDeliveredID, info.FirstEntry.ID) &&
				info.Length >= r.max && idLess(st.GapMark, info.FirstEntry.ID) {
				r.gap(c, g.LastDeliveredID, info.FirstEntry.ID, "stream cap reached while no relay was storing")
				upd.GapMark = info.FirstEntry.ID
			}
		}
	}
	// 2) Valkey restarted: whatever was written after the last fsync is gone.
	// What was stored is safe; the interval from the last stored entry to the
	// restart is uncertain (with AOF everysec: about a second).
	if st.RunID != "" && runID != "" && st.RunID != runID && stored != "" {
		// up to the restart (uptime is in whole seconds: round up)
		to := strconv.FormatInt(time.Now().Add(-time.Duration(upS)*time.Second).UnixMilli()+1000, 10) + "-0"
		r.gap(c, stored, to, "valkey restarted; entries after the last stored one may be lost")
	}
	lag, backlog := r.lag(c)
	if err := r.db.SaveRelayState(c, name, upd, lag, backlog); err != nil {
		r.log.Warn("relay state", "err", err)
	}
}

func (r *relay) gap(ctx context.Context, from, to, why string) {
	r.log.Warn("event stream gap", "from", from, "to", to, "why", why)
	data, _ := json.Marshal(map[string]any{"from_id": from, "to_id": to,
		"from": time.UnixMilli(idMs(from)).UTC().Format(time.RFC3339Nano),
		"to":   time.UnixMilli(idMs(to)).UTC().Format(time.RFC3339Nano), "reason": why})
	row := []store.DomainRow{{ID: "gap:" + from + ":" + to, Type: events.EventsGap,
		SchemaVer: events.SchemaVer, Time: time.Now().UTC(), Data: data}}
	// Must reach the feed: retried like any batch (a gap is likeliest while
	// Postgres is down). Then remembered, so it is reported once.
	for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := r.db.InsertDomainEvents(c, row)
		if err == nil {
			err = r.db.SaveRelayState(c, name, store.RelayState{GapMark: to}, -1, -1)
		}
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}
		r.log.Error("store gap event, retrying", "in", wait, "err", err)
		time.Sleep(wait)
	}
}

func (r *relay) serveHealth() {
	reg := &metrics.Registry{}
	reg.Process()
	reg.CounterFunc("relay_stored_total", "Events stored in domain_events.", func() float64 { return float64(r.stored.Load()) })
	reg.CounterFunc("relay_duplicates_total", "Entries already stored (repeats after a failure).", func() float64 { return float64(r.dupes.Load()) })
	reg.Gauge("relay_healthy", "1 while storing works.", func() float64 {
		if r.healthy.Load() {
			return 1
		}
		return 0
	})
	var lagMs, backlog atomic.Int64
	reg.Gauge("relay_lag_seconds", "Age of the oldest event not stored yet (the feed's delay).", func() float64 {
		return float64(lagMs.Load()) / 1000
	})
	reg.Gauge("relay_backlog_entries", "Events in the stream not stored yet; must stay well below EVENTS_STREAM_MAX.", func() float64 {
		return float64(backlog.Load())
	})
	http.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		ctx, cancel := context.WithTimeout(q.Context(), 2*time.Second)
		l, b := r.lag(ctx)
		cancel()
		lagMs.Store(l)
		backlog.Store(b)
		reg.Handler().ServeHTTP(w, q)
	}))
	http.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		code := 200
		if !r.healthy.Load() {
			code = 503
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"stored": r.stored.Load(), "duplicates": r.dupes.Load()})
	})
	_ = (&http.Server{Addr: env("RELAY_HEALTH_ADDR", ":8091"), ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
}

func idParts(id string) (int64, int64) {
	a, b, _ := strings.Cut(id, "-")
	ms, _ := strconv.ParseInt(a, 10, 64)
	n, _ := strconv.ParseInt(b, 10, 64)
	return ms, n
}

func idMs(id string) int64 { ms, _ := idParts(id); return ms }

// idLess: stream id a < b ("" is the smallest).
func idLess(a, b string) bool {
	am, an := idParts(a)
	bm, bn := idParts(b)
	return am < bm || (am == bm && an < bn)
}
