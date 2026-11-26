// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Command engine is the cloud ISO 15118 SECC of a deployment.
//
// It answers V2G requests tunnelled by devices over MQTT. The working state
// of a session lives in Valkey (ADR-002): every message loads the session,
// steps the state machine and stores it back with compare-and-set, so any
// engine of the deployment can take the next message, and an engine can be
// restarted or added at any time. Postgres keeps the history.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/lib/pq"

	"github.com/chargetronic/imatra.iso15118_server/engine/internal/events"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/exi"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/secc"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/store"
	"github.com/chargetronic/imatra.iso15118_server/engine/internal/tunnel"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envF(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func nowUS() int64 { return time.Now().UnixMicro() }

type engine struct {
	log    *slog.Logger
	db     *store.Store
	client mqtt.Client
	def    secc.Config
	env    secc.Env
	dev    tunnel.Config // device settings by default (T, TTL, ...)
	renew  time.Duration // how often the TTL is renewed while a pair is active

	mu     sync.Mutex
	expiry map[string]*time.Timer // per device: re-check when its limit expires
	final  finalReads
	supply supplyState

	coord    *coordinator        // several instances: leader, device locks (coord.go)
	st       stateStore          // where running sessions live: Valkey (statestore.go)
	w        *workers            // per-device workers (workers.go)
	senders  *workers            // activation sends (activations.go)
	c        *caches             // hot-path caches, journal mode (caches.go)
	tb       *telemetryBatcher   // batched metering writes
	twin     chan store.EventRow // twin timeline rows for twinWriter
	sent     chan sentMark       // commands sent, for twinWriter
	acks     chan store.Ack      // device confirmations for ackWriter
	feed     chan events.Event   // feed events for feedWriter
	bg       *store.Store        // background writes: history, own small pool (store.OpenBackground)
	draining atomic.Bool         // SIGTERM: not ready, finishing what was taken (drain)
	pres     *presenceWriter     // device presence, batched (presence.go)
	blocked  *blocklist          // devices blocked by the operator (block.go)
	reg      registry            // the service layer's device registry (registry.go)
	iso20    bool                // ISO20_ENABLED: offer ISO 15118-20 AC on TLS 1.3 connections (ADR-004)
}

func main() {
	// "engine healthcheck": used by the container healthcheck (no curl in the image).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		r, err := c.Get("http://127.0.0.1:8090/health")
		if err != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable connect_timeout=5",
		env("POSTGRES_HOST", "postgres"), env("POSTGRES_PORT", "5432"), os.Getenv("POSTGRES_DB"),
		os.Getenv("POSTGRES_USER"), os.Getenv("POSTGRES_PASSWORD"))
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

	bg, err := store.OpenBackground(dsn)
	if err != nil {
		log.Error("database (background pool)", "err", err)
		os.Exit(1)
	}
	e := &engine{
		log:  log,
		db:   db,
		bg:   bg,
		twin: make(chan store.EventRow, 20000),
		acks: make(chan store.Ack, 50000),
		feed: make(chan events.Event, 50000),
		sent: make(chan sentMark, 20000),
		env:  secc.DefaultEnv(),
		def: secc.Config{
			EVSEID:          env("EVSE_ID", "FI*CTR*E0001"),
			MaxCurrentA:     envF("EVSE_MAX_CURRENT_A", 32),
			NominalVoltageV: envF("EVSE_NOMINAL_VOLTAGE_V", 230),
			Phases:          int(envF("EVSE_PHASES", 3)),
		},
		dev: tunnel.Config{
			SampleS:       int(envF("DEVICE_LOOP_SAMPLE_S", 30)),
			TTLS:          int(envF("DEVICE_TTL_S", 1200)),
			HeartbeatS:    int(envF("DEVICE_HEARTBEAT_S", 60)),
			TermBackupS:   int(envF("DEVICE_TERM_BACKUP_S", 5)),
			Trace:         env("DEVICE_TRACE", "true") == "true",
			CacheEnabled:  env("DEVICE_CACHE", "true") == "true",
			DigitalMode:   env("DEVICE_DIGITAL_MODE", "true") == "true",
			HLCWaitS:      int(envF("DEVICE_HLC_WAIT_S", 30)),
			TelemetryS:    int(envF("DEVICE_TELEMETRY_S", 60)),
			SoCFinal:      env("DEVICE_SOC_FINAL", "true") == "true",
			SoCFinalWaitS: int(envF("DEVICE_SOC_FINAL_WAIT_S", 20)),
		},
		renew:   time.Duration(envF("DEVICE_TTL_RENEW_S", 300) * float64(time.Second)),
		expiry:  map[string]*time.Timer{},
		final:   newFinalReads(),
		supply:  supplyState{last: map[string]time.Time{}},
		c:       newCaches(),
		blocked: &blocklist{set: map[string]struct{}{}},
		iso20:   env("ISO20_ENABLED", "false") == "true",
	}
	if e.st, err = newStateStore(db, log); err != nil {
		log.Error("state store", "err", err)
		os.Exit(1)
	}
	e.w = newWorkers(int(envF("ENGINE_WORKERS", 32)), 4096, log)
	// Activation sends have their own queue (ACT_SENDERS): a stage does not
	// wait behind the EVs' requests, nor they behind it. Per device in order;
	// against a step of the same session running meanwhile, the version
	// check of the state (ADR-001) decides.
	e.senders = newWorkers(int(envF("ACT_SENDERS", 64)), 8192, log)
	e.coord = newCoordinator(e)
	e.tb = e.startTelemetryBatcher()
	e.pres = e.startPresenceWriter()

	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%s", env("MQTT_HOST", "mqtt"), env("MQTT_PORT", "1883"))).
		SetClientID(clientID(e.coord.id)).
		SetUsername("server").SetPassword(os.Getenv("MQTT_SERVER_PASSWORD")).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).SetConnectRetryInterval(2 * time.Second).
		SetKeepAlive(30 * time.Second).
		// One goroutine hands messages to the device workers in arrival order;
		// the handlers themselves only enqueue (workers.go).
		SetOrderMatters(true)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Info("connected to broker")
		// Every handler runs on the worker of its device (workers.go):
		// one device in order, devices in parallel.
		// MQTT_SHARE_GROUP: several engine instances share the device topics
		// ($share/<group>/...), the broker gives each message to one of them.
		c.Subscribe(shared(tunnel.Root+"/+/v2g/up"), 0, e.byDevice(e.onUp))
		c.Subscribe(shared(tunnel.Root+"/+/trace"), 0, e.byDevice(e.onTrace))
		c.Subscribe(shared(tunnel.Root+"/+/status"), 1, e.byDeviceSlow(e.onStatus))
		c.Subscribe(shared(tunnel.Root+"/+/cp"), 1, e.byDevice(e.onCP))
		c.Subscribe(shared(tunnel.TelemetryTopic()), 1, e.byDevice(e.onTelemetry))
		c.Subscribe(shared(tunnel.AckTopic()), 1, e.onAck) // not on the device workers: confirmations queue apart
		if e.coord.isLeader() {
			go e.publishBlocked() // retained; the broker may have restarted empty
		}
		e.reg.mu.Lock()
		e.reg.subscribed = false // a clean session: the subscription is gone
		e.reg.mu.Unlock()
		go e.registrySync(false)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) { log.Warn("broker connection lost", "err", err) })
	e.client = guardedClient{mqtt.NewClient(opts), e} // nothing to blocked devices (block.go)
	e.client.Connect()

	e.startMetrics()
	// Internal health endpoint for the compose healthcheck; /live, /ready and
	// /metrics for an orchestrator (metrics.go).
	go func() {
		e.probes()
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			st := map[string]string{"db": "ok", "mqtt": "ok"}
			code := 200
			if err := db.Ping(ctx); err != nil {
				st["db"], code = "error", 503
			}
			if !e.client.IsConnectionOpen() {
				st["mqtt"], code = "error", 503
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(st)
		})
		_ = (&http.Server{Addr: env("HEALTH_ADDR", ":8090"), ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
	}()

	go e.twinWriter()
	go e.ackWriter()
	go e.feedWriter()
	go e.journalLoop()
	go e.renewLoop()
	go e.retentionLoop()
	go e.listenLimits(dsn)
	go e.activations(dsn) // scheduler on the leader, sends on every instance
	go e.refreshAll()     // after a restart: limit expiry timers and pairs of running sessions
	go e.blockedLoop()    // operator's local block (A5): list, broker ACL via svc/blocked
	go e.registryLoop()   // the service layer's device registry, read by the leader (block D)
	log.Info("engine started", "evse", e.def, "device", e.dev, "ttl_renew", e.renew)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	e.drain()
	e.client.Disconnect(500)
}

// drain: a scale-down or a rolling update without lost EV requests. Not
// ready first (the orchestrator stops sending traffic), then off the shared
// subscriptions (the broker gives new messages to the other instances), then
// finish what was taken: the device workers, the activation sends, the
// background writes; then the leader lease and the membership go, so another
// instance takes over at once instead of after the lease TTL.
// ENGINE_DRAIN_S (15 s) bounds it; the orchestrator's grace period must be longer.
func (e *engine) drain() {
	e.draining.Store(true)
	limit := time.Now().Add(time.Duration(envF("ENGINE_DRAIN_S", 15) * float64(time.Second)))
	if e.client.IsConnectionOpen() {
		topics := []string{shared(tunnel.Root + "/+/v2g/up"), shared(tunnel.Root + "/+/trace"), shared(tunnel.Root + "/+/status"), shared(tunnel.Root + "/+/cp"),
			shared(tunnel.TelemetryTopic()), shared(tunnel.AckTopic())}
		if t := e.client.Unsubscribe(topics...); !t.WaitTimeout(3 * time.Second) {
			e.log.Warn("drain: unsubscribe not confirmed")
		}
	}
	for time.Now().Before(limit) && e.w.busy.Load()+e.senders.busy.Load() > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	for time.Now().Before(limit) && len(e.twin)+len(e.acks)+len(e.feed)+len(e.sent) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(700 * time.Millisecond) // the batch writers' last round (100-500 ms)
	e.coord.leave()
	e.log.Info("drained", "workers_left", e.w.busy.Load(), "sends_left", e.senders.busy.Load())
}

func (e *engine) publish(device string, d *tunnel.Down) {
	d.V = tunnel.Version
	d.TSrvTx = nowUS()
	b, _ := json.Marshal(d)
	e.client.Publish(tunnel.DownTopic(device), 0, false, b)
}

func (e *engine) onUp(_ mqtt.Client, m mqtt.Message) {
	at := recvAt(m)
	tSrvRx := nowUS()
	device := tunnel.DeviceFromTopic(m.Topic())
	var up tunnel.Up
	if err := json.Unmarshal(m.Payload(), &up); err != nil || device == "" || up.Conn == "" {
		e.log.Warn("bad tunnel message", "topic", m.Topic(), "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := e.log.With("device", device, "conn", up.Conn, "seq", up.Seq)

	if up.Event == "closed" {
		reason := "connection closed"
		if up.Reason != "" {
			reason = up.Reason
		}
		if err := e.st.Close(ctx, device, up.Conn, reason); err != nil {
			log.Error("close session", "err", err)
		}
		e.closeOpen(device, up.Conn)
		log.Info("EV connection closed")
		return
	}
	tm := newPhases(tSrvRx, up.TUpTx)
	if err := e.ensureDevice(ctx, device); err != nil {
		log.Error("device", "err", err)
		return
	}
	tm.mark("device")
	sess, err := e.st.Load(ctx, device, up.Conn)
	if err == nil && sess == nil {
		st0 := secc.NewState(time.Now())
		if up.Event == "open" {
			st0.TLS = up.TLS // ISO 15118-20 needs TLS 1.3 to the EV (ADR-004)
		}
		if up.Event == "" && len(up.EXI) > 0 && secc.ResumePT(st0, ptOf(up), up.EXI) {
			// A request in the middle of a session the server does not know:
			// its state was lost (Valkey restart). Answered by the standard
			// (V2G2-460 / V2G2-756), not by closing without an answer.
			log.Warn("request of a session the server does not know: state lost, answered per ISO 15118-2", "seq", up.Seq)
			mLost.Inc()
		}
		sess, err = e.st.Create(ctx, device, up.Conn, st0)
		tm.mark("create")
	} else {
		tm.mark("load")
	}
	if errors.Is(err, errEnded) {
		// A late request of a session another instance already closed (the
		// EV was unplugged): no answer, no event (ADR-001).
		log.Info("request after session end, dropped", "seq", up.Seq)
		return
	}
	if err != nil {
		log.Error("load session", "err", err)
		return
	}
	e.setOpen(device, sess.ID, up.Conn)
	if up.Event == "open" {
		transport := "tcp"
		if up.TLS != "" {
			transport = "tls"
		}
		e.addEvent(ctx, device, sess.ID, secc.Event{Type: "connection.open",
			Data: map[string]any{"transport": transport, "tls": up.TLS}})
		return
	}
	if len(up.EXI) == 0 {
		return
	}
	if up.Sampled {
		// Answered from the cache already: only a liveness sign of the loop
		// (journal only: at scale the TTL and the heartbeat carry liveness).
		// ISO 15118-20: an EV that asks for MeterInfo from now on gets it in
		// the cached answer too (V2G20-1082).
		if secc.IsISO20(ptOf(up)) && !sess.State.MeterRequested {
			if q, err := secc.LoopSample20(ptOf(up), up.EXI); err == nil && q.MeterInfoRequested {
				e.meterPair(ctx, device, true)
			}
		}
		if !e.journal(device) {
			return
		}
		data := map[string]any{"seq": up.Seq, "up_ms": float64(tSrvRx-up.TEVRx) / 1000}
		if secc.IsISO20(ptOf(up)) {
			// ISO 15118-20: the sample carries the EV's current report (SoC,
			// in Dynamic mode its needs); journalled with the liveness sign.
			if q, err := secc.LoopSample20(ptOf(up), up.EXI); err == nil {
				data["ev"] = q
			}
		}
		e.addEvent(ctx, device, sess.ID, secc.Event{Type: "loop.alive", Data: data})
		return
	}
	if pt := ptOf(up); pt != exi.PayloadEXI && !secc.IsISO20(pt) {
		log.Warn("unexpected payload type", "pt", fmt.Sprintf("0x%04x", up.PT))
		return
	}

	cfg := e.stationConfig(ctx, device)
	tm.mark("config")

	// Step, pair, save. With several engine instances another one may have
	// changed this session meanwhile (a limit refresh): the save then fails
	// on the version, and the step is redone on the fresh state. Nothing has
	// been sent yet, so redoing it is safe.
	var res *secc.Result
	var stepErr error
	var cl *tunnel.CachePair
	for attempt := 0; ; attempt++ {
		if up.Plug != "" {
			sess.State.Plug = up.Plug // the plug-in this session runs in (ADR-003)
		}
		if up.LastTS > sess.State.LastTS {
			sess.State.LastTS = up.LastTS // the device's cached answers (V2G20-1537)
		}
		res, stepErr = secc.StepPT(sess.State, cfg, e.env, ptOf(up), up.EXI)
		if stepErr != nil {
			break
		}
		// A changed Charging Loop answer goes to the device with this response.
		cl = e.tag(cfg, e.pair(sess.State, cfg, false))
		// State first, then the answer: a crash after publishing must not lose
		// the step the EV already saw. The step's feed events are saved with
		// the state, in one step.
		err = e.st.Save(ctx, sess, events.FromStep(device, sess.ID, sess.State, res.Events, at)...)
		if !errors.Is(err, errConflict) || attempt == 3 {
			break
		}
		if sess, err = e.st.Load(ctx, device, up.Conn); err != nil || sess == nil {
			if errors.Is(err, errEnded) || sess == nil {
				log.Info("session ended during the step, dropped", "seq", up.Seq)
			} else {
				log.Error("reload session", "err", err)
			}
			return
		}
	}
	if stepErr != nil {
		// Undecodable or out-of-scope message: no valid response exists, so
		// the connection is closed (the EV starts over).
		sess.State.Phase, sess.State.EndReason = secc.PhaseEnded, "engine error: "+stepErr.Error()
		_ = e.st.Save(ctx, sess, events.Event{Type: events.V2GEnded, Device: device, SessionID: sess.ID,
			Time: at, Plug: sess.State.Plug, Data: map[string]any{"reason": sess.State.EndReason, "failed": true}})
		e.closeOpen(device, up.Conn)
		e.publish(device, &tunnel.Down{Conn: up.Conn, Seq: up.Seq, PT: ptOf(up), Close: true, TSrvRx: tSrvRx})
		_, _ = e.journalMessage(ctx, device, &store.Message{SessionID: sess.ID, Conn: up.Conn, Seq: up.Seq,
			Direction: "ev_to_server", Source: "device", Type: "undecodable", EXI: up.EXI,
			Decoded: map[string]string{"error": stepErr.Error()}, Payload: len(up.EXI), Wire: up.WireBytes,
			TEVRx: up.TEVRx, TUpTx: up.TUpTx, TSrvRx: tSrvRx})
		log.Error("step failed, closing", "err", stepErr)
		mFailed.Inc()
		return
	}

	if err != nil {
		log.Error("save session", "err", err)
		return
	}
	tm.mark("step+save")
	if sess.State.MeterRequested && sess.State.Protocol == exi.NamespaceISO20AC {
		e.markMeter(ctx, device)
	}
	down := &tunnel.Down{Conn: up.Conn, Seq: up.Seq, PT: res.PT, EXI: res.EXI, Close: res.Close, TSrvRx: tSrvRx, CL: cl}
	e.publish(device, down)
	tm.mark("publish")
	tm.warnIfSlow(log, res.RequestType)

	// Journal after the answer is on its way: off the critical path.
	// JOURNAL_MODE=flag: only devices with debug_journal (caches.go).
	reqID, err := e.journalMessage(ctx, device, &store.Message{SessionID: sess.ID, Conn: up.Conn, Seq: up.Seq,
		Direction: "ev_to_server", Source: "device", Type: res.RequestType, EXI: up.EXI, Decoded: res.Request,
		Payload: len(up.EXI), Wire: up.WireBytes, TEVRx: up.TEVRx, TUpTx: up.TUpTx, TSrvRx: tSrvRx})
	if err != nil {
		log.Error("journal request", "err", err)
	}
	if _, err := e.journalMessage(ctx, device, &store.Message{SessionID: sess.ID, Conn: up.Conn, Seq: up.Seq,
		Direction: "server_to_ev", Source: "server", Type: res.ResponseType, EXI: res.EXI, Decoded: res.Response,
		Payload: len(res.EXI), Wire: len(res.EXI) + 8, TSrvRx: tSrvRx, TSrvTx: down.TSrvTx, PairID: &reqID}); err != nil {
		log.Error("journal response", "err", err)
	}
	for _, ev := range res.Events {
		if ev.Type == "session.failed" {
			mFailed.Inc()
		}
		if ev.Type == "soc.read" {
			e.onSoCRead(ctx, device, sess.ID, ev)
		}
		e.log.Info("event", "device", device, "type", ev.Type)
		e.addEvent(ctx, device, sess.ID, ev)
	}
	if res.Close && sess.State.Protocol == exi.NamespaceDIN {
		e.afterSoC(ctx, device, sess.ID)
	}
	log.Info("exchange", "req", res.RequestType, "res", res.ResponseType,
		"phase", sess.State.Phase, "srv_us", down.TSrvTx-tSrvRx, "cl", cl != nil)
}

// afterSoC: the DIN session of a DIN-only EV (it offers no ISO 15118-2 AC)
// gave the SoC and is over: basic charging on the station's PWM (via X1 3 s).
// A pulse back to 5 % would only repeat the DIN session for ever (found on
// a test charger, 03.11.2026). An EV that offers ISO 15118-2 never gets here: its
// session is AC, and a digital session is not left for a SoC read (08.11.2026).
func (e *engine) afterSoC(ctx context.Context, device, sessionID string) {
	conf := e.dev
	if _, err := e.db.DeviceSettings(ctx, device, &conf); err != nil || !conf.DigitalMode {
		return
	}
	cmd := tunnel.CPSet{V: tunnel.Version, Mode: "transit"}
	b, _ := json.Marshal(cmd)
	e.client.Publish(tunnel.CPSetTopic(device), 1, false, b)
	e.addEvent(ctx, device, sessionID, secc.Event{Type: "cp.mode",
		Data: map[string]any{"mode": "transit", "reason": "SoC read done; the EV offers no ISO 15118-2 AC: basic charging on the PWM"}})
	e.log.Info("DIN-only EV: basic charging after the SoC read", "device", device)
}

// onCP journals the device's CP reports; plug-in and unplug are events of
// their own.
func (e *engine) onCP(_ mqtt.Client, m mqtt.Message) {
	device := tunnel.DeviceFromTopic(m.Topic())
	var st tunnel.CPState
	if device == "" || json.Unmarshal(m.Payload(), &st) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.ensureDevice(ctx, device); err != nil {
		return
	}
	if m.Retained() {
		return // replay of the last state after the engine (re)subscribed: already journalled
	}
	sid := e.openSessionID(ctx, device)
	data := map[string]any{"mode": st.Mode, "out": st.Out, "ev": st.EV, "station": st.Station, "power": st.Power}
	if st.Note != "" {
		data["note"] = st.Note
	}
	if st.Slac != "" {
		data["slac"] = st.Slac
		if st.EVMac != "" {
			data["ev_mac"] = st.EVMac
		}
	}
	if st.Mode == "basic" || st.Mode == "transit" {
		data["duty"] = st.Duty
		if st.LimitA != nil {
			data["limit_a"] = *st.LimitA
		}
	}
	e.addEvent(ctx, device, sid, secc.Event{Type: "cp.state", Data: data})
	at := recvAt(m)
	e.emit(ctx, events.Event{Type: events.CPChanged, Device: device, SessionID: sid, Time: at, Plug: st.Plug,
		Data: cpData(st)})
	e.watchStop(device, st)
	if st.Mode == "basic" && st.Out == "mask" && st.LimitA != nil {
		// The basic limit reached the CP: this confirms the operator command as
		// well (a device without cmd/ack; with it, whichever comes first
		// counts and the other changes nothing). Only if the CP holds this
		// command's current, not the one before it.
		if cfg := e.stationConfig(ctx, device); cfg.CommandID != 0 && math.Abs(*st.LimitA-cfg.EffectiveCurrentA()) < 0.05 {
			e.confirm(ctx, device, sid, cfg.CommandID, "cp_report", "applied", at, st.Plug)
		}
	}
	if st.Event == "plugged" && e.plugRepeated(ctx, device) {
		st.Event = "" // the same plug-in reported again (see plugRepeated)
	}
	if st.Event == "unplugged" {
		e.plugForget(ctx, device)
	}
	if st.Event == "plugged" || st.Event == "unplugged" {
		ed := map[string]any{"ev": st.EV}
		if st.Event == "unplugged" {
			e.finalUnplugged(ctx, device, sid)
			e.closeOpen(device, "")
			if n, err := e.st.CloseAll(ctx, device, "EV unplugged"); err == nil && n > 0 {
				e.log.Warn("sessions left open closed on unplug", "device", device, "n", n)
			}
			// energy of this plug-in (all charging, digital and basic): from the
			// hot record (counter increments, powerScript), no Postgres on the
			// device's worker (ADR-002). The energy of each digital session is
			// the projector's (v2g.ended). With the record
			// lost, from the telemetry table as before.
			if wh, s, ok := e.plugEnergy(ctx, device); ok {
				ed["energy_wh"], ed["plugged_s"] = float64(int(wh*100))/100, s
			} else if since, err := e.db.PluggedSince(ctx, device); err == nil && !since.IsZero() {
				if wh, err := e.db.Energy(ctx, device, since); err == nil {
					ed["energy_wh"] = float64(int(wh*100)) / 100
					ed["plugged_s"] = int(time.Since(since).Seconds())
				}
			}
			if st.PluggedMs != nil { // the device's own count: delivery delays do not change it (ADR-003)
				ed["plugged_s"] = int(*st.PluggedMs / 1000)
			}
		}
		e.addEvent(ctx, device, sid, secc.Event{Type: "ev." + st.Event, Data: ed})
		if st.Event == "plugged" {
			e.emit(ctx, events.Event{Type: events.SessionStarted, Device: device, Time: at, Plug: st.Plug,
				Data: map[string]any{"cp": st.EV}})
			go e.hsLocation(device) // the activation pool's record gets the location
		} else {
			d := map[string]any{"reason": "ev_unplugged"}
			if v, ok := ed["energy_wh"]; ok {
				d["energy_wh"], d["duration_s"], d["energy_quality"] = v, ed["plugged_s"], energyQuality(e.deviceRules(ctx, device))
			}
			if st.PluggedMs != nil {
				d["plugged_ms"], d["duration_s"] = *st.PluggedMs, int(*st.PluggedMs/1000)
			}
			e.emit(ctx, events.Event{Type: events.SessionEnded, Device: device, Time: at, Plug: st.Plug, Data: d})
		}
		e.log.Info("EV "+st.Event, "device", device, "energy_wh", ed["energy_wh"])
	}
}

// onTelemetry stores the device's metering (current, energy).
func (e *engine) onTelemetry(_ mqtt.Client, m mqtt.Message) {
	device := tunnel.DeviceFromTopic(m.Topic())
	var t tunnel.Telemetry
	if device == "" || json.Unmarshal(m.Payload(), &t) != nil || len(t.IA) == 0 {
		return
	}
	var all map[string]any
	_ = json.Unmarshal(m.Payload(), &all)
	extra := map[string]any{}
	for _, k := range []string{"ev", "cp_mode", "cp_out", "station_duty", "limit_a", "seq"} {
		if v, ok := all[k]; ok && v != nil {
			extra[k] = v
		}
	}
	data, _ := json.Marshal(extra)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.ensureDevice(ctx, device); err != nil {
		return
	}
	sid := e.openSessionID(ctx, device)
	e.tb.in <- store.TelemetryRow{Device: device, SessionID: sid, DeviceUS: t.T, At: recvAt(m), IA: t.IA, PW: t.PTotal, EWh: t.EWh, Data: data}
	// order of the readings: boot and seq of the device (ADR-003); a device
	// before 0.3.2 sends no boot: arrival order
	ord := order{boot: t.Boot, seq: t.Seq}
	if t.Boot == "" {
		ord.seq = recvAt(m).UnixMicro()
	}
	if _, err := e.st.Power(ctx, device, sid, ord, recvAt(m), t.Plug, t.PTotal, t.EWh); err != nil {
		e.log.Warn("power event", "device", device, "err", err)
	}
	e.meterPair(ctx, device, false) // ISO 15118-20 MeterInfo: the new energy into the pair
}

// meterPair refreshes the MeterInfo of a running ISO 15118-20 session whose
// EV asked for it (V2G20-1082): the energy of the digital session from the
// device's readings goes into the device's pair (a push when the whole Wh
// changed). request: the EV has just asked for it (a cached loop sample).
func (e *engine) meterPair(ctx context.Context, device string, request bool) {
	vk := e.vk()
	if vk == nil {
		return
	}
	if !request {
		// most telemetry: no -20 session with MeterInfo on this device
		if n, err := vk.Exists(ctx, meterKey(device)).Result(); err != nil || n == 0 {
			return
		}
	}
	open, err := e.st.Open(ctx, device)
	if err != nil || len(open) == 0 {
		return
	}
	o := open[0]
	if o.State.Protocol != exi.NamespaceISO20AC || (!o.State.MeterRequested && !request) {
		return
	}
	o.State.MeterRequested = true
	e.markMeter(ctx, device)
	if wh, ok := e.digitalEnergyWh(ctx, device); ok {
		o.State.MeterWh = wh
	}
	cfg := e.stationConfig(ctx, device)
	cl := e.tag(cfg, e.pair(o.State, cfg, false))
	if cl == nil && !request {
		return // the same answer: nothing to send
	}
	if err := e.st.Save(ctx, &o.Session); err != nil {
		return // a conflict: a step of the session saved meanwhile, the next reading retries
	}
	if cl != nil {
		b, _ := json.Marshal(tunnel.Push{V: tunnel.Version, Conn: o.Conn, CL: cl})
		e.client.Publish(tunnel.PushTopic(device), 1, false, b)
	}
}

// meterKey marks a device whose running -20 session wants MeterInfo, so that
// telemetry looks at its session only then.
func meterKey(device string) string { return "m20:" + device }

func (e *engine) markMeter(ctx context.Context, device string) {
	if vk := e.vk(); vk != nil {
		vk.Set(ctx, meterKey(device), 1, 48*time.Hour)
	}
}

// digitalEnergyWh: the energy since the digital session started, from the
// plug-in record (pool.go: eplug, e2g).
func (e *engine) digitalEnergyWh(ctx context.Context, device string) (float64, bool) {
	vk := e.vk()
	if vk == nil {
		return 0, false
	}
	r, err := vk.HMGet(ctx, "hs:"+device, "eplug", "e2g").Result()
	if err != nil || r[0] == nil {
		return 0, false
	}
	ep, _ := strconv.ParseFloat(fmt.Sprint(r[0]), 64)
	e2g := 0.0
	if r[1] != nil {
		e2g, _ = strconv.ParseFloat(fmt.Sprint(r[1]), 64)
	}
	return math.Max(0, ep-e2g), true
}

// ptOf: the request's V2GTP payload type; a device before the field sends
// none, and then only ISO 15118-2 / DIN 70121 (0x8001).
func ptOf(up tunnel.Up) uint16 {
	if up.PT == 0 {
		return exi.PayloadEXI
	}
	return up.PT
}

// stationConfig: limits and identity the responses are built from. Every
// answer to the EV (from the server or from the cache) must use the same one.
// The engine's ISO 15118-20 setting goes with it (not stored with the rest).
func (e *engine) stationConfig(ctx context.Context, device string) secc.Config {
	cfg := e.stationConfigStored(ctx, device)
	cfg.ISO20 = e.iso20
	return cfg
}

func (e *engine) stationConfigStored(ctx context.Context, device string) secc.Config {
	vk := e.vk()
	if vk != nil { // shared hot state (hotstate.go)
		if cfg, ok := e.hotGet(ctx, vk, device); ok {
			if cfg.CommandID != 0 {
				e.watchExpiry(device, cfg.LimitUntil)
			}
			return cfg
		}
	} else if cfg, ok := e.cachedStation(device); ok {
		return cfg
	}
	cfg, err := e.db.DeviceConfig(ctx, device, e.def)
	if err != nil {
		e.log.Warn("device config, using defaults", "device", device, "err", err)
		return e.stationDefaults(ctx, cfg, device) // not cached: retry next time
	}
	cfg = e.stationDefaults(ctx, cfg, device)
	if vk != nil {
		e.hotPut(ctx, vk, device, cfg)
	} else {
		e.storeStation(device, cfg)
	}
	return cfg
}

// stationDefaults: EVSEID per device and the active operator limit. Cached
// with the rest by stationConfig; refresh() drops the cache first, and every
// change of a limit or a configuration goes through refresh() (NOTIFY limits /
// config, limit expiry), so a cached value is never older than the last change.
func (e *engine) stationDefaults(ctx context.Context, cfg secc.Config, device string) secc.Config {
	cfg = applyRules(cfg, e.deviceRules(ctx, device)) // business rules: rating, installation, phases
	if cfg.EVSEID == e.def.EVSEID && os.Getenv("EVSE_ID") == "" {
		cfg.EVSEID = "FI*CTR*E" + strings.ToUpper(strings.ReplaceAll(device, "-", ""))
	}
	if l, err := e.db.ActiveLimit(ctx, device); err != nil {
		e.log.Warn("active limit", "device", device, "err", err)
	} else if l != nil {
		cfg.LimitA, cfg.CommandID, cfg.LimitSource, cfg.LimitUntil, cfg.LimitPrio = l.LimitA, l.CommandID, l.Source, l.ExpiresAt, l.Priority
		e.watchExpiry(device, l.ExpiresAt)
	}
	return cfg
}

// listenLimits: the API notifies "limits" with the device id on every new or
// cancelled limit (the device gets a new Charging Loop answer at once) and
// "config" on every new device configuration (published to the device).
func (e *engine) listenLimits(dsn string) {
	l := pq.NewListener(dsn, time.Second, 30*time.Second, func(ev pq.ListenerEventType, err error) {
		if err != nil {
			e.log.Warn("limits listener", "err", err)
		}
	})
	for _, ch := range []string{"limits", "config", "blocked"} {
		if err := l.Listen(ch); err != nil {
			e.log.Error("listen", "channel", ch, "err", err)
			return
		}
	}
	for n := range l.Notify {
		if n == nil { // reconnected: notifications may be lost, re-check all
			e.dropSettings("")
			e.refreshAll()
			go e.reloadBlocked("listener reconnected")
			continue
		}
		switch n.Channel {
		case "blocked":
			go e.reloadBlocked("notify " + n.Extra)
		case "limits":
			device := n.Extra
			if e.vk() == nil {
				e.dropStation(device) // this instance's memory; with Valkey the owner's refresh rebuilds the shared record
			}
			if !e.coord.owns(device) {
				continue // another engine instance reacts
			}
			e.w.do(device, func() { e.refresh(device, "limit changed") }) // in parallel over devices
		case "config":
			device := n.Extra
			e.dropSettings(device) // every instance: its own memory
			if e.vk() == nil {
				e.dropStation(device) // with Valkey: the owner's refresh below
			}
			if !e.coord.owns(device) {
				continue
			}
			e.w.do(device, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if ok, _ := e.db.DeviceExists(ctx, device); !ok {
					return // not a device of this deployment
				}
				e.publishConfig(ctx, device)
				// station section (max current, phases) may have changed: a running
				// session gets it through a new cache pair; no push if the answer is the same
				e.refresh(device, stationChanged)
			})
		}
	}
}

// watchExpiry re-checks the device right after its limit ends.
func (e *engine) watchExpiry(device string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.expiry[device]; t != nil {
		t.Stop()
	}
	e.expiry[device] = time.AfterFunc(time.Until(at)+100*time.Millisecond, func() {
		if e.coord.owns(device) {
			e.w.do(device, func() { e.refresh(device, "limit expired") })
		} else {
			e.dropStation(device)
		}
	})
}

func (e *engine) refreshAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open, err := e.st.Open(ctx, "")
	if err != nil {
		return
	}
	// Devices with an open session, and devices with an active limit but no
	// session: basic-mode (PWM) limits have no session to find them by
	// (review 09.11.2026, R2).
	devices := make([]string, 0, len(open))
	for _, o := range open {
		devices = append(devices, o.Device)
	}
	if limited, err := e.db.LimitDevices(ctx); err == nil {
		devices = append(devices, limited...)
	} else {
		e.log.Warn("catch-up: limited devices", "err", err)
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if !seen[d] && e.coord.owns(d) {
			seen[d] = true
			device := d
			e.w.do(device, func() { e.refresh(device, "catch-up") })
		}
	}
}

// refresh gives the device a new Charging Loop answer when the limits
// changed it (digital session in the loop). Outside the loop the limit is
// picked up by the next response the engine builds.
// stationChanged: refresh reason after a new device configuration (UI).
const stationChanged = "station config changed"

func (e *engine) refresh(device, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if why != "activation" && why != whyResend { // an activation wrote its limit into the hot state already
		e.dropStation(device) // a limit or the configuration changed: read it again
	}
	tm := newPhases(0, 0) // activation sends: a sample of the steps is logged
	defer func() {
		if (why == "activation" || why == whyResend) && rand.IntN(50) == 0 {
			e.log.Info("activation send steps", append([]any{"device", device}, tm.parts...)...)
		}
	}()
	// once: the same settings for power/set and the pair; with Valkey in one
	// call together with the open session (an activation send: 2 calls in all)
	cfg, pre, havePre := e.configAndOpen(ctx, device, why)
	tm.mark("config")
	e.sendPower(device, cfg) // basic mode: the device applies it when on the station PWM side
	tm.mark("power")
	if why != "catch-up" { // a catch-up re-sends, nothing changed
		e.feedLater(e.limitEvent(device, cfg, why)) // off the send path
	}
	var o store.OpenSession
	var cl *tunnel.CachePair
	for attempt := 0; ; attempt++ {
		open, err := pre, error(nil)
		if attempt > 0 || !havePre {
			open, err = e.st.Open(ctx, device)
		}
		tm.mark("open")
		if err != nil || len(open) == 0 {
			return
		}
		o = open[0]
		if cl = e.tag(cfg, e.pair(o.State, cfg, why == whyResend)); cl == nil {
			return
		}
		tm.mark("pair")
		err = e.st.Save(ctx, &o.Session)
		tm.mark("save")
		if err == nil {
			break
		}
		if !errors.Is(err, errConflict) || attempt == 3 {
			e.log.Error("save session", "err", err)
			return
		}
	}
	b, _ := json.Marshal(tunnel.Push{V: tunnel.Version, Conn: o.Conn, CL: cl})
	e.client.Publish(tunnel.PushTopic(device), 1, false, b)
	tm.mark("publish")
	data := map[string]any{"reason": why, "evse_max_current_a": cfg.EffectiveCurrentA(), "pair_ver": cl.Ver}
	kind := "command.applied"
	if why == stationChanged {
		kind = "config.applied" // station settings from the UI, not an operator command
	}
	if cfg.CommandID != 0 {
		data["command_id"], data["limit_a"], data["source"] = cfg.CommandID, cfg.LimitA, cfg.LimitSource
		e.markSent(cfg.CommandID) // sent; the device confirms with cmd/ack
	}
	e.addEvent(ctx, device, o.ID, secc.Event{Type: kind, Data: data})
	e.log.Info("new loop answer pushed", "device", device, "why", why, "evse_max_current_a", cfg.EffectiveCurrentA())
}

// tag: the pair carries the id of the operator command whose limit it holds,
// so the device's ack confirms that command.
func (e *engine) tag(cfg secc.Config, cl *tunnel.CachePair) *tunnel.CachePair {
	if cl != nil {
		cl.CommandID = cfg.CommandID // from the station cache: no query
	}
	return cl
}

// onAck: the device took a command over (tunnel.Ack).
func (e *engine) onAck(_ mqtt.Client, m mqtt.Message) {
	device := tunnel.DeviceFromTopic(m.Topic())
	var a tunnel.Ack
	if device == "" || json.Unmarshal(m.Payload(), &a) != nil || a.CommandID == 0 {
		return // a pair from the station settings only: nothing to confirm
	}
	via := map[string]string{"pair": "loop", "power": "pwm"}[a.Kind]
	if via == "" {
		return
	}
	// reaction time = arrival of the ack - sending of the command, both on the
	// server's clock (ADR-003); onAck runs in the MQTT callback, so this is
	// the arrival, not a queue
	e.confirm(context.Background(), device, "", a.CommandID, via, a.Result, recvAt(m), a.Plug)
}

// confirm: queues a device confirmation for ackWriter. Confirmations are
// off the send path (activations rework, 10.11.2026): they are written in
// batches, and the reaction time comes from the device's clock, so writing
// them a little later changes no figure.
func (e *engine) confirm(_ context.Context, device, _ string, commandID int64, via, result string, at time.Time, plug string) {
	applied := result == "installed" || result == "applied"
	if !applied && result != "held" {
		return // stale, rejected, cleared
	}
	a := store.Ack{CommandID: commandID, Device: device, Via: via, Applied: applied, At: at, Plug: plug}
	select {
	case e.acks <- a:
	default: // full: write this one now
		e.writeAcks([]store.Ack{a})
	}
}

// ackWriter: confirmations every 150 ms (or 2000 at once) in one statement;
// the first confirmation of a command is news: limit.applied with the
// reaction time; repeats, stale and reordered acks are not (ADR-001, rule 4).
func (e *engine) ackWriter() {
	var buf []store.Ack
	tick := time.NewTicker(150 * time.Millisecond)
	for {
		select {
		case a := <-e.acks:
			if buf = append(buf, a); len(buf) >= 2000 {
				e.writeAcks(buf)
				buf = buf[:0]
			}
		case <-tick.C:
			if len(buf) > 0 {
				e.writeAcks(buf)
				buf = buf[:0]
			}
		}
	}
}

func (e *engine) writeAcks(acks []store.Ack) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done, err := e.db.AckCommands(ctx, acks)
	if err != nil {
		e.log.Error("command acks", "acks", len(acks), "err", err)
		return
	}
	now := time.Now().UTC()
	plugs := map[int64]string{}
	for _, a := range acks {
		if a.Plug != "" {
			plugs[a.CommandID] = a.Plug
		}
	}
	evs := make([]events.Event, 0, len(done))
	for _, c := range done {
		e.addEvent(ctx, c.Device, "", secc.Event{Type: "command.acked", Data: map[string]any{
			"command_id": c.CommandID, "via": c.Via, "reaction_ms": c.ReactionMs}})
		evs = append(evs, events.Event{Type: events.LimitApplied, Device: c.Device, Time: now, Plug: plugs[c.CommandID],
			Data: map[string]any{"command_id": c.CommandID, "via": c.Via, "reaction_ms": c.ReactionMs}})
	}
	e.emit(ctx, evs...)
	if len(done) > 0 {
		e.log.Info("commands confirmed", "acks", len(acks), "first", len(done))
	}
}

// limitEvent: the current that holds for the device now and why. Emitted by
// the owner of the device only (refresh), so once per change.
func (e *engine) limitEvent(device string, cfg secc.Config, why string) events.Event {
	d := map[string]any{"limit_a": cfg.EffectiveCurrentA(), "source": "station", "reason": why}
	if cfg.CommandID != 0 {
		d["source"], d["command_id"], d["requested_a"] = cfg.LimitSource, cfg.CommandID, cfg.LimitA
		d["until"] = cfg.LimitUntil.UTC().Format(time.RFC3339)
	}
	return events.Event{Type: events.LimitChanged, Device: device, SessionID: sessionLater,
		Time: time.Now().UTC(), Data: d}
}

// publishPower sends the active limit for basic mode (0 = none). The device
// keeps it while a digital session runs and applies it on the PWM side
// (transit): no HLC, or after a local termination.
func (e *engine) publishPower(ctx context.Context, device string) {
	e.sendPower(device, e.stationConfig(ctx, device))
}

func (e *engine) sendPower(device string, cfg secc.Config) {
	p := tunnel.PowerSet{V: tunnel.Version, Ver: nowUS(), TTLS: e.dev.TTLS, TSrv: nowUS()}
	if cfg.CommandID != 0 {
		p.CommandID, p.LimitA = cfg.CommandID, cfg.EffectiveCurrentA()
		p.DurationS = int(time.Until(cfg.LimitUntil).Seconds()) + 1
	}
	b, _ := json.Marshal(p)
	e.client.Publish(tunnel.PowerSetTopic(device), 1, false, b)
	e.log.Info("power/set", "device", device, "limit_a", p.LimitA, "duration_s", p.DurationS)
}

// pair returns the Charging Loop pair of the session when it differs from
// what the device has (or always, with force), and records it in the state.
func (e *engine) pair(st *secc.State, cfg secc.Config, force bool) *tunnel.CachePair {
	if !e.dev.CacheEnabled {
		return nil
	}
	p, err := secc.CachePairOf(st, cfg, time.Now())
	if err != nil {
		e.log.Error("cache pair", "err", err)
		return nil
	}
	if p == nil || (!force && p.Key == st.CLRes) {
		return nil
	}
	st.CLRes = p.Key // ISO 15118-2: the answer's hex; -20: the same without its time stamp
	req, _ := hex.DecodeString(st.CLReq)
	cl := &tunnel.CachePair{Ver: nowUS(), Req: req, Res: p.Res, Term: p.Term, TTLS: e.dev.TTLS}
	// one algorithm on the device for both versions (ADR-005)
	cl.PT, cl.ReqBits, cl.TermPT = p.PT, p.ReqBits, p.TermPT
	if p.TS > 0 { // ISO 15118-20: the device writes the time stamp
		at := p.ResTS
		cl.Kind, cl.ResTS, cl.TermTS, cl.TS = "prefix", &at, p.TermTS, p.TS
	}
	return cl
}

// onStatus: when a device (re)connects, give it its settings and the
// pair of its running session (the broker may have lost or never had it).
func (e *engine) onStatus(c mqtt.Client, m mqtt.Message) {
	e.onPresence(c, m)
	device := tunnel.DeviceFromTopic(m.Topic())
	var st tunnel.Status
	if json.Unmarshal(m.Payload(), &st) != nil || device == "" || !st.Online {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !m.Retained() {
		e.supplyCheck(ctx, device, m.Payload()) // rule "supply" (rules.go)
	}
	if st.Event != "connect" {
		return
	}
	e.publishConfig(ctx, device)
	e.publishPower(ctx, device)

	open, err := e.st.Open(ctx, device)
	if err != nil {
		e.log.Error("open sessions", "device", device, "err", err)
		return
	}
	for _, o := range open {
		cfg := e.stationConfig(ctx, device)
		if cl := e.tag(cfg, e.pair(o.State, cfg, true)); cl != nil {
			_ = e.st.Save(ctx, &o.Session) // a conflict only means a newer pair went out meanwhile
			b, _ := json.Marshal(tunnel.Push{V: tunnel.Version, Conn: o.Conn, CL: cl})
			e.client.Publish(tunnel.PushTopic(device), 1, false, b)
			e.log.Info("pair re-sent after reconnect", "device", device, "conn", o.Conn)
		}
		break // one EV per device: the newest open session
	}
}

// onPresence keeps devices.online / status / last_seen_at from the retained
// heartbeat and the broker's LWT ({"online": false}) of devices and
// simulators. (Was the ingest service, removed 01.11.2026.)
func (e *engine) onPresence(_ mqtt.Client, m mqtt.Message) {
	parts := strings.Split(m.Topic(), "/")
	if len(parts) != 3 || parts[1] == "" {
		return
	}
	if parts[0] != tunnel.Root || !json.Valid(m.Payload()) {
		return
	}
	var st tunnel.Status
	_ = json.Unmarshal(m.Payload(), &st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.pres.put(store.Presence{Device: parts[1], Online: st.Online, Status: append([]byte(nil), m.Payload()...),
		At: time.Now()}, st.Event == "connect" && !m.Retained())
	// Feed: a (re)connect and the broker's last will; not the heartbeats, not
	// a retained replay after the engine subscribed.
	if !m.Retained() && (st.Event == "connect" || !st.Online) {
		ev := events.Event{Type: events.DeviceOffline, Device: parts[1], Time: time.Now().UTC(), Data: map[string]any{}}
		if st.Online {
			ev.Type = events.DeviceOnline
			var all map[string]any
			_ = json.Unmarshal(m.Payload(), &all)
			for _, k := range []string{"fw", "firmware", "link", "transport", "rssi"} {
				if v, ok := all[k]; ok {
					ev.Data.(map[string]any)[k] = v
				}
			}
		}
		e.emit(ctx, ev)
	}
}

// publishConfig sends the device its settings (retained): server defaults
// overlaid with the latest device configuration from the UI.
func (e *engine) publishConfig(ctx context.Context, device string) {
	conf, rev, ok := e.cachedSettings(device)
	if !ok {
		conf = e.dev
		var err error
		if rev, err = e.db.DeviceSettings(ctx, device, &conf); err != nil {
			e.log.Warn("device config, using defaults", "device", device, "err", err)
		} else {
			e.storeSettings(device, conf, rev)
		}
	}
	conf.V, conf.Ver, conf.Rev = tunnel.Version, nowUS(), rev
	b, _ := json.Marshal(conf)
	e.client.Publish(tunnel.ConfigTopic(device), 1, true, b)
	e.log.Info("config published", "device", device, "rev", rev)
}

// renewLoop renews the TTL of every device that holds a pair. The TTL is
// the device's guard against a dead server: only the engine renews it.
func (e *engine) renewLoop() {
	if e.renew <= 0 {
		return
	}
	for range time.Tick(e.renew) {
		if !e.coord.isLeader() {
			continue // one instance renews for the whole fleet
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		open, err := e.st.Open(ctx, "")
		cancel()
		if err != nil {
			e.log.Error("renew: open sessions", "err", err)
			continue
		}
		done := map[string]bool{}
		devices := []string{}
		for _, o := range open {
			if o.State.CLRes != "" && !done[o.Device] {
				done[o.Device] = true
				devices = append(devices, o.Device)
			}
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		limited, _ := e.db.LimitDevices(ctx) // basic-mode limits hold by the same TTL
		cancel()
		for _, d := range limited {
			if !done[d] {
				done[d] = true
				devices = append(devices, d)
			}
		}
		for _, d := range devices {
			b, _ := json.Marshal(tunnel.Ctrl{V: tunnel.Version, Cmd: "ttl_renew", TTLS: e.dev.TTLS, TSrv: nowUS()})
			e.client.Publish(tunnel.CtrlTopic(d), 1, false, b)
		}
	}
}

// emit: an event for the operator's feed.
// sessionLater: feedLater fills in the device's open session (one Valkey
// read less on the send path).
const sessionLater = "?"

// feedLater: an event for the feed, written by feedWriter within 100 ms.
func (e *engine) feedLater(ev events.Event) {
	select {
	case e.feed <- ev:
	default: // full: now
		if ev.SessionID == sessionLater {
			ev.SessionID = e.openSessionID(context.Background(), ev.Device)
		}
		e.emit(context.Background(), ev)
	}
}

func (e *engine) feedWriter() {
	var buf []events.Event
	tick := time.NewTicker(100 * time.Millisecond)
	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for i := range buf {
			if buf[i].SessionID == sessionLater {
				buf[i].SessionID = e.openSessionID(ctx, buf[i].Device)
			}
		}
		e.emit(ctx, buf...)
		buf = buf[:0]
	}
	for {
		select {
		case ev := <-e.feed:
			if buf = append(buf, ev); len(buf) >= 2000 {
				flush()
			}
		case <-tick.C:
			if len(buf) > 0 {
				flush()
			}
		}
	}
}

func (e *engine) emit(ctx context.Context, evs ...events.Event) {
	if err := e.st.Emit(ctx, evs...); err != nil {
		e.log.Error("feed event", "err", err)
	}
}

func cpData(st tunnel.CPState) map[string]any {
	d := map[string]any{"ev": st.EV, "mode": st.Mode, "out": st.Out}
	if st.Mode == "basic" || st.Mode == "transit" {
		d["duty"] = st.Duty
	}
	if st.LimitA != nil {
		d["limit_a"] = *st.LimitA
	}
	if len(st.Station) > 0 {
		d["station"] = st.Station // the station's own PWM as the device sees it
	}
	return d
}

// twinKept: twin timeline rows written for every device: the plug-in
// boundaries (time of the plug-in without Valkey) and the SoC reads of a
// DIN-only EV (final SoC read, socfinal.go). A few per plug-in.
var twinKept = map[string]bool{"ev.plugged": true, "ev.unplugged": true, "soc.read": true,
	"soc.final.requested": true, "soc.final": true, "cp.mode": true}

// addEvent: one row of the twin timeline, written in batches every 100 ms by
// twinWriter (off the device workers: an activation stage of thousands of
// devices otherwise waited on one insert per device); written at once if the
// buffer is full.
func (e *engine) addEvent(ctx context.Context, device, sessionID string, ev secc.Event) {
	// JOURNAL_MODE=flag (scale): the twin timeline of a device without the
	// debug journal holds only what the engine reads back (twinKept); the
	// rest is in the operator feed already (10.11.2026).
	if !twinKept[ev.Type] && !e.journal(device) {
		return
	}
	select {
	case e.twin <- store.EventRow{Device: device, SessionID: sessionID, Kind: ev.Type, Data: ev.Data, At: time.Now()}:
	default:
		if err := e.db.AddEvent(ctx, device, sessionID, ev.Type, ev.Data); err != nil {
			e.log.Error("event", "err", err)
		}
	}
}

// markSent: the command went out (limits.applied_at), batched like the twin.
func (e *engine) markSent(commandID int64) {
	select {
	case e.sent <- sentMark{commandID, time.Now()}:
	default:
		_ = e.db.MarkApplied(context.Background(), commandID)
	}
}

type sentMark struct {
	id int64
	at time.Time
}

// twinWriter flushes the twin events and sent marks every 100 ms.
func (e *engine) twinWriter() {
	var evs []store.EventRow
	var ids []int64
	var ats []time.Time
	tick := time.NewTicker(100 * time.Millisecond)
	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if len(evs) > 0 {
			if err := e.bg.AddEvents(ctx, evs); err != nil {
				e.log.Error("twin events", "rows", len(evs), "err", err)
			}
			evs = evs[:0]
		}
		if len(ids) > 0 {
			if err := e.db.MarkAppliedBatch(ctx, ids, ats); err != nil {
				e.log.Error("sent marks", "rows", len(ids), "err", err)
			}
			ids, ats = ids[:0], ats[:0]
		}
	}
	for {
		select {
		case r := <-e.twin:
			if evs = append(evs, r); len(evs) >= 2000 {
				flush()
			}
		case m := <-e.sent:
			ids, ats = append(ids, m.id), append(ats, m.at)
			if len(ids) >= 2000 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// journalMessage writes one V2G message to the journal when the device is
// journalled (JOURNAL_MODE, caches.go); returns 0 otherwise.
func (e *engine) journalMessage(ctx context.Context, device string, m *store.Message) (int64, error) {
	if !e.journal(device) {
		return 0, nil
	}
	if m.PairID != nil && *m.PairID == 0 {
		m.PairID = nil
	}
	return e.db.AddMessage(ctx, m)
}

func (e *engine) onTrace(_ mqtt.Client, m mqtt.Message) {
	device := tunnel.DeviceFromTopic(m.Topic())
	if !e.journal(device) {
		return // traces belong to the message journal
	}
	var t tunnel.Trace
	if err := json.Unmarshal(m.Payload(), &t); err != nil || t.Conn == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if t.FromCache {
		e.journalCacheHit(ctx, device, &t)
		return
	}
	sess, err := e.st.Load(ctx, device, t.Conn)
	if err != nil || sess == nil {
		return
	}
	if err := e.db.AddTrace(ctx, sess.ID, t.Seq, t.TDownRx, t.TEVTx); err != nil {
		e.log.Error("trace", "err", err)
	}
}

// Names of the frames the device answered itself (it does not decode them):
// a cache hit is a ChargingStatus; the termination scenario goes by position.
var termTypes = []string{"ChargingStatus", "PowerDelivery", "SessionStop"}

func (e *engine) journalCacheHit(ctx context.Context, device string, t *tunnel.Trace) {
	sess, err := e.st.Load(ctx, device, t.Conn)
	if err != nil || sess == nil {
		return
	}
	typ, dec := "ChargingStatus", map[string]any{"from_cache": true}
	if pv := sess.State.CLMaxCurrent; pv != nil {
		dec["evse_max_current"] = pv
	}
	if t.Term {
		var n int
		fmt.Sscanf(t.Note, "term:%d", &n)
		if n >= 0 && n < len(termTypes) {
			typ = termTypes[n]
		}
		dec["termination"] = t.Note
	}
	reqID, err := e.db.AddMessage(ctx, &store.Message{SessionID: sess.ID, Conn: t.Conn, Seq: t.Seq,
		Direction: "ev_to_server", Source: "device", Type: typ + "Req", Decoded: dec,
		Payload: t.ReqBytes, Wire: t.ReqBytes + 8, TEVRx: t.TEVRx})
	if err != nil {
		e.log.Error("journal cache hit", "err", err)
		return
	}
	id, err := e.db.AddMessage(ctx, &store.Message{SessionID: sess.ID, Conn: t.Conn, Seq: t.Seq,
		Direction: "server_to_ev", Source: "cache", Type: typ + "Res", Decoded: dec,
		Payload: t.ResBytes, Wire: t.ResBytes + 8, PairID: &reqID})
	if err == nil {
		_ = e.db.AddTrace(ctx, sess.ID, t.Seq, 0, t.TEVTx)
	}
	_ = id
}

// shared: the device topics as a shared subscription (MQTT_SHARE_GROUP,
// default "engine"): the broker gives each message to one engine instance;
// with one instance it gets them all. The same with one engine or several,
// so adding one is `docker compose up -d --scale engine=2` (OSS scope, B4).
func shared(topic string) string {
	return "$share/" + env("MQTT_SHARE_GROUP", "engine") + "/" + topic
}

// clientID: unique per instance (instances share the topics).
func clientID(instance string) string {
	return "server-engine-" + instance
}

// phases: where the time of one EV request went, logged when it is slow
// (SLOW_REQUEST_MS, default 500; ISO 15118-2 gives the station 1.5 s).
// "queued": from the device's send to the handler's start (broker, MQTT
// client, device worker queue); meaningful only with synchronised clocks.
type phases struct {
	start, last time.Time
	queued      time.Duration
	parts       []any
}

var slowRequest = time.Duration(envF("SLOW_REQUEST_MS", 500)) * time.Millisecond

func newPhases(srvRxUS, upTxUS int64) *phases {
	p := &phases{start: time.Now(), last: time.Now()}
	if upTxUS > 0 && srvRxUS > upTxUS {
		p.queued = time.Duration(srvRxUS-upTxUS) * time.Microsecond
	}
	return p
}

func (p *phases) mark(name string) {
	now := time.Now()
	p.parts = append(p.parts, name+"_ms", now.Sub(p.last).Milliseconds())
	p.last = now
}

func (p *phases) warnIfSlow(log *slog.Logger, req string) {
	total := time.Since(p.start)
	if total+p.queued < slowRequest {
		return
	}
	args := append([]any{"request", req, "handler_ms", total.Milliseconds(), "queued_ms", p.queued.Milliseconds()}, p.parts...)
	log.Warn("slow EV request", args...)
}
