// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Command fleetgen is the virtual device fleet for load tests of the
// server: N devices speak the same MQTT protocol as real devices, their
// EVs run real ISO 15118-2 AC sessions (requests encoded with libcbv2g) or
// charge on the PWM only. It measures what the server side does under load:
// answer latency per V2G message, how long an operator command takes to
// reach a stage of devices, how long a fleet needs to come back after the
// broker drops it.
//
// Scenarios (combine freely):
//
//	fleet + plug-ins   -n, -plug-frac, -plug-spread, -charge   (night background, evening peak)
//	activation stage   -activate-at, -activate-n, -activate-limit (operator API, one call per device)
//	mass reconnect     -reconnect-at, -reconnect-jitter
//
// Devices fleet-NNNNN connect with a client certificate each (CN = id),
// signed here with a test CA (-client-ca, -client-ca-key). A shared password
// (-password) is for a broker with password accounts, not a deployment. See README.md.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type options struct {
	broker     string
	caFile     string
	insecure   bool
	prefix     string
	password   string
	clientCA   string
	clientKey  string
	certDays   int
	n, start   int
	profiles   string
	connRate   float64
	keepaliveS int

	plugFrac   float64
	plugSpread time.Duration
	settle     time.Duration // pause after connecting, before plug-ins (the server's connect-time sync)
	charge     time.Duration
	duration   time.Duration
	evMaxA     float64
	loopMs     int
	sampleS    int
	telemetryS int
	timeoutS   float64

	apiURL         string
	apiKey         string
	activateAt     time.Duration
	activateN      int
	activateLimitA float64
	activateDurS   int
	activateConc   int
	activatePool   bool
	activateGroups int     // >0: POST /v1/activations, this many at once over disjoint devices
	activateSpread float64 // s, spread of the stage
	activateHold   time.Duration

	reconnectAt     time.Duration
	reconnectJitter time.Duration

	clockSkew  time.Duration // ADR-003 check: the devices' wall clock ("t") off by this
	lateUnplug time.Duration // ADR-003 check: the unplug report sent this long after the unplug
	legacy     bool          // no plug-in ids (a device before 0.3.2)

	report string
}

func main() {
	o := &options{}
	flag.StringVar(&o.broker, "broker", "tcp://127.0.0.1:1883", "MQTT broker: tcp://host:1883 or ssl://host:8883")
	flag.StringVar(&o.caFile, "ca", "", "CA file for ssl:// (default: system roots)")
	flag.BoolVar(&o.insecure, "insecure", false, "do not verify the broker certificate (test hosts)")
	flag.StringVar(&o.prefix, "prefix", "fleet-", "device id prefix; ids are <prefix>NNNNN")
	flag.StringVar(&o.clientCA, "client-ca", "", "test CA certificate: each device gets a client certificate (CN = id) signed with it")
	flag.StringVar(&o.clientKey, "client-ca-key", "", "key of -client-ca (a TEST CA only)")
	flag.IntVar(&o.certDays, "cert-days", 7, "validity of the devices' certificates, days")
	flag.StringVar(&o.password, "password", os.Getenv("FLEET_PASSWORD"), "shared MQTT password (or FLEET_PASSWORD): a broker with password accounts, not a deployment")
	flag.IntVar(&o.n, "n", 100, "number of devices")
	flag.IntVar(&o.start, "start", 1, "first device number (to split a fleet over several generators)")
	flag.StringVar(&o.profiles, "profiles", "iso=0.7,pwm=0.3", "EV mix")
	flag.Float64Var(&o.connRate, "conn-rate", 200, "MQTT connections per second at start")
	flag.IntVar(&o.keepaliveS, "keepalive", 60, "MQTT keepalive, s")
	flag.Float64Var(&o.plugFrac, "plug-frac", 0.65, "share of devices that get an EV during the run")
	flag.DurationVar(&o.plugSpread, "plug-spread", time.Minute, "plug-ins spread evenly over this time after connecting")
	flag.DurationVar(&o.settle, "settle", 0, "pause after all devices connected, before heartbeats and plug-ins (a fleet at rest, not a reconnect wave)")
	flag.DurationVar(&o.charge, "charge", 10*time.Minute, "how long each EV charges")
	flag.DurationVar(&o.duration, "duration", 5*time.Minute, "run time after all devices connected")
	flag.Float64Var(&o.evMaxA, "ev-max-a", 16, "EV current per phase without a limit, A")
	flag.IntVar(&o.loopMs, "loop-ms", 1000, "EV Charging Loop period, ms")
	flag.IntVar(&o.sampleS, "sample-s", 0, "override loop_sample_s T from the server config (0 = take the server's)")
	flag.IntVar(&o.telemetryS, "telemetry-s", 0, "override telemetry_s from the server config (0 = take the server's)")
	flag.Float64Var(&o.timeoutS, "timeout", 0, "wait for a V2G answer, s (0: as the EV per ISO 15118-2 table 109 — 2 s, PowerDelivery 5 s)")
	flag.StringVar(&o.apiURL, "api", "", "operator API base, e.g. https://host (activation scenario)")
	flag.StringVar(&o.apiKey, "api-key", os.Getenv("FLEET_API_KEY"), "X-API-Key (or FLEET_API_KEY)")
	flag.DurationVar(&o.activateAt, "activate-at", 0, "send the activation stage this long after start (0 = none)")
	flag.IntVar(&o.activateN, "activate-n", 500, "devices in the activation stage (charging ones first)")
	flag.Float64Var(&o.activateLimitA, "activate-limit", 8, "limit per phase, A")
	flag.IntVar(&o.activateDurS, "activate-dur", 900, "limit duration, s")
	flag.IntVar(&o.activateConc, "activate-conc", 32, "parallel API calls")
	flag.BoolVar(&o.activatePool, "activate-pool", false, "group activation by criteria (charging, count): the server picks the devices from its pool")
	flag.IntVar(&o.activateGroups, "activate-groups", 0, "0: one limit call per device; N: N activations (POST /v1/activations) at once over disjoint devices")
	flag.Float64Var(&o.activateSpread, "activate-spread", 0, "group activation: spread of the stage, s")
	flag.DurationVar(&o.activateHold, "activate-hold", 3*time.Second, "group activation: keep it this long after the deliveries, then DELETE")
	flag.DurationVar(&o.reconnectAt, "reconnect-at", 0, "drop all MQTT connections this long after start (0 = none)")
	flag.DurationVar(&o.reconnectJitter, "reconnect-jitter", time.Minute, "devices come back at random within this time")
	flag.DurationVar(&o.clockSkew, "clock-skew", 0, "devices' wall clock off by this (e.g. -30s); the server must not depend on it (ADR-003)")
	flag.DurationVar(&o.lateUnplug, "late-unplug", 0, "send the unplug report this long after the unplug (offline device); the plug-in keeps its real duration")
	flag.BoolVar(&o.legacy, "legacy", false, "no plug-in ids in the messages (a device before 0.3.2)")
	flag.StringVar(&o.report, "report", "fleetgen-report.json", "report file")
	flag.Parse()
	var fca *fleetCA
	if o.clientCA != "" {
		var err error
		if fca, err = loadFleetCA(o.clientCA, o.clientKey, o.certDays); err != nil {
			log.Fatal(err)
		}
	} else if o.password == "" {
		log.Fatal("no -client-ca / -client-ca-key (or, legacy listener only, -password)")
	}
	mix, err := parseMix(o.profiles)
	if err != nil {
		log.Fatal(err)
	}

	var tlsConf *tls.Config
	if strings.HasPrefix(o.broker, "ssl://") || strings.HasPrefix(o.broker, "tls://") {
		tlsConf = &tls.Config{InsecureSkipVerify: o.insecure, MinVersion: tls.VersionTLS12}
		if o.caFile != "" {
			pem, err := os.ReadFile(o.caFile)
			if err != nil {
				log.Fatal(err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsConf.RootCAs = pool
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; log.Print("interrupted: writing the report"); cancel() }()

	m := newMetrics()
	devs := make([]*device, o.n)
	for i := range devs {
		dc := tlsConf
		if fca != nil {
			if dc == nil {
				log.Fatal("-client-ca needs an ssl:// broker")
			}
			id := fmt.Sprintf("%s%05d", o.prefix, o.start+i)
			cert, err := fca.issue(id)
			if err != nil {
				log.Fatal(err)
			}
			dc = tlsConf.Clone()
			dc.Certificates = []tls.Certificate{cert}
		}
		devs[i] = newDevice(o.start+i, o, m, pickProfile(mix), dc)
	}

	// 1. connect at -conn-rate
	log.Printf("connecting %d devices at %.0f/s to %s", o.n, o.connRate, o.broker)
	t0 := time.Now()
	var wg sync.WaitGroup
	var connected int64
	gap := time.Duration(float64(time.Second) / o.connRate)
	for _, d := range devs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(d *device) {
			defer wg.Done()
			if err := d.connect(); err != nil {
				log.Printf("%s: %v", d.id, err)
				return
			}
			atomic.AddInt64(&connected, 1)
		}(d)
		time.Sleep(gap)
	}
	wg.Wait()
	log.Printf("connected %d/%d in %.1f s", connected, o.n, time.Since(t0).Seconds())
	m.observe("fleet.connect_all", time.Since(t0))
	if o.settle > 0 {
		log.Printf("settling %s before plug-ins", o.settle)
		time.Sleep(o.settle)
	}

	// 2. heartbeats and plug-ins
	runCtx, stop := context.WithTimeout(ctx, o.duration)
	defer stop()
	for _, d := range devs {
		if d.client == nil || !d.client.IsConnected() {
			continue
		}
		go d.heartbeat(runCtx)
		if rand.Float64() < o.plugFrac {
			at := time.Duration(rand.Int63n(int64(o.plugSpread) + 1))
			wg.Add(1)
			go func(d *device) {
				defer wg.Done()
				select {
				case <-runCtx.Done():
					return
				case <-time.After(at):
				}
				// charge until the end of the run at most, then unplug cleanly
				left := time.Until(deadlineOf(runCtx))
				dur := o.charge
				if left-10*time.Second < dur {
					dur = left - 10*time.Second
				}
				if dur > 0 {
					d.session(ctx, dur)
				}
			}(d)
		}
	}

	var act *ActivationReport
	var rec *ReconnectReport
	if o.activateAt > 0 && o.apiURL != "" {
		go func() {
			select {
			case <-runCtx.Done():
			case <-time.After(o.activateAt):
				if o.activateGroups > 0 {
					act = activateGroup(runCtx, o, m, devs)
				} else {
					act = activate(runCtx, o, m, devs)
				}
			}
		}()
	}
	if o.reconnectAt > 0 {
		go func() {
			select {
			case <-runCtx.Done():
			case <-time.After(o.reconnectAt):
				rec = reconnect(runCtx, o, devs)
			}
		}()
	}

	// progress line every 10 s
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(10 * time.Second):
				log.Printf("sessions %d started / %d ended, v2g timeouts %d, cache hits %d",
					m.count("session.started"), m.count("session.ended"), sumPrefix(m, "v2g.timeout."), m.count("loop.cache_hit"))
			}
		}
	}()

	<-runCtx.Done()
	log.Print("run time over: waiting for sessions to close")
	waitTimeout(&wg, 60*time.Second)
	time.Sleep(2 * time.Second) // let the last activation deliveries land
	for _, d := range devs {
		if d.client != nil && d.client.IsConnected() {
			d.client.Disconnect(100)
		}
	}

	r := m.report(map[string]any{"n": o.n, "start": o.start, "profiles": o.profiles, "broker": o.broker,
		"plug_frac": o.plugFrac, "plug_spread": o.plugSpread.String(), "charge": o.charge.String(),
		"duration": o.duration.String(), "loop_ms": o.loopMs, "activate_n": o.activateN,
		"reconnect_at": o.reconnectAt.String(), "date": time.Now().Format(time.RFC3339)})
	r.Activation, r.Reconnect = act, rec
	r.print(os.Stdout)
	if err := r.save(o.report); err != nil {
		log.Print(err)
	} else {
		log.Printf("report: %s", o.report)
	}
}

// activate sends one limit per device through the operator API (as the
// operator would today, one call each) and measures until each device got
// its new Charging Loop pair (ISO) or power/set (PWM).
func activate(ctx context.Context, o *options, m *metrics, devs []*device) *ActivationReport {
	var pick []*device
	for _, d := range devs { // charging ones first: they are the ones a stage limits
		d.mu.Lock()
		in := d.inSession && (d.pairReq != nil || d.profile == ProfilePWM)
		d.mu.Unlock()
		if in {
			pick = append(pick, d)
		}
	}
	if len(pick) > o.activateN {
		pick = pick[:o.activateN]
	}
	rep := &ActivationReport{Devices: len(pick)}
	log.Printf("activation: %d devices, %.0f A for %d s", len(pick), o.activateLimitA, o.activateDurS)
	var mu sync.Mutex
	var first, lastSent, lastDelivered time.Time
	done := make(chan struct{}, len(pick)+1)
	onDeliver := func(d *device, at time.Time) {
		mu.Lock()
		rep.Delivered++
		if at.After(lastDelivered) {
			lastDelivered = at
		}
		mu.Unlock()
		done <- struct{}{}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	sem := make(chan struct{}, o.activateConc)
	var wg sync.WaitGroup
	first = time.Now()
	for _, d := range pick {
		wg.Add(1)
		sem <- struct{}{}
		go func(d *device) {
			defer wg.Done()
			defer func() { <-sem }()
			body, _ := json.Marshal(map[string]any{"limit_a": o.activateLimitA, "duration_s": o.activateDurS})
			req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.apiURL, "/")+"/v1/devices/"+d.id+"/limit", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", o.apiKey)
			sent := time.Now()
			d.mu.Lock()
			d.cmdAt, d.onDeliver = sent, func(d *device, at time.Time) {
				m.observe("activation.per_device", at.Sub(sent))
				onDeliver(d, at)
			}
			d.mu.Unlock()
			resp, err := client.Do(req)
			m.observe("activation.api_call", time.Since(sent))
			mu.Lock()
			if err != nil || resp.StatusCode != 202 {
				rep.APIErrors++
				if err == nil {
					log.Printf("activation %s: HTTP %d", d.id, resp.StatusCode)
				}
			} else {
				rep.APIAccepted++
			}
			if time.Now().After(lastSent) {
				lastSent = time.Now()
			}
			mu.Unlock()
			if resp != nil {
				resp.Body.Close()
			}
		}(d)
	}
	wg.Wait()
	// wait for deliveries (or 60 s)
	deadline := time.After(60 * time.Second)
	for got := 0; got < rep.APIAccepted; {
		select {
		case <-done:
			got++
		case <-deadline:
			got = rep.APIAccepted
		case <-ctx.Done():
			got = rep.APIAccepted
		}
	}
	mu.Lock()
	defer mu.Unlock()
	rep.SendSpanS = lastSent.Sub(first).Seconds()
	if !lastDelivered.IsZero() {
		rep.DeliverSpanS = lastDelivered.Sub(first).Seconds()
	}
	rep.PerDevice = statOf(m.samplesOf("activation.per_device"))
	log.Printf("activation: %d delivered, last %.2f s after the first call", rep.Delivered, rep.DeliverSpanS)

	// Leave no limits behind: a limit still active in the next run would be
	// "the same answer" for the EV, the engine pushes nothing and the
	// delivery is never seen (found 08.11.2026).
	clear := func(d *device) {
		req, _ := http.NewRequest("DELETE", strings.TrimRight(o.apiURL, "/")+"/v1/devices/"+d.id+"/limit", nil)
		req.Header.Set("X-API-Key", o.apiKey)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	cleanup := make(chan *device)
	var cw sync.WaitGroup
	for i := 0; i < o.activateConc; i++ {
		cw.Add(1)
		go func() {
			defer cw.Done()
			for d := range cleanup {
				d.mu.Lock()
				d.cmdAt, d.onDeliver = time.Time{}, nil
				d.mu.Unlock()
				clear(d)
			}
		}()
	}
	for _, d := range pick {
		cleanup <- d
	}
	close(cleanup)
	cw.Wait()
	log.Printf("activation: limits cleared on %d devices", len(pick))
	return rep
}

// activateGroup: the stage as N group commands (POST /v1/activations)
// over disjoint parts of the charging devices, sent at the same moment;
// per device: POST -> new pair or power/set on the device. Ended with
// DELETE /v1/activations/{id} (the limits in force before apply again).
func activateGroup(ctx context.Context, o *options, m *metrics, devs []*device) *ActivationReport {
	var pick []*device
	for _, d := range devs {
		d.mu.Lock()
		in := d.inSession && (d.pairReq != nil || d.profile == ProfilePWM)
		d.mu.Unlock()
		if in {
			pick = append(pick, d)
		}
	}
	want := len(pick)
	if want > o.activateN {
		want = o.activateN
	}
	k := o.activateGroups
	if o.activatePool {
		// the server chooses from its pool (selection by criteria): every
		// device in a session may get the limit, want of them will
		k = 1
	} else {
		pick = pick[:want]
	}
	rep := &ActivationReport{Devices: want}
	log.Printf("activation: %d devices in %d group command(s), %.0f A, spread %.1f s", len(pick), k, o.activateLimitA, o.activateSpread)
	var mu sync.Mutex
	var lastDelivered time.Time
	done := make(chan struct{}, len(pick)+1)
	client := &http.Client{Timeout: 30 * time.Second}
	run := strconv.FormatInt(time.Now().UnixMilli(), 36)
	ids := make([]string, k)
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < k; g++ {
		var part []*device
		for i := g; i < len(pick); i += k {
			part = append(part, pick[i])
		}
		ids[g] = fmt.Sprintf("fleetgen-%s-%d", run, g)
		names := make([]string, len(part))
		for i, d := range part {
			names[i] = d.id
			d.mu.Lock()
			d.cmdAt, d.onDeliver = start, func(d *device, at time.Time) {
				m.observe("activation.per_device", at.Sub(start))
				mu.Lock()
				rep.Delivered++
				if at.After(lastDelivered) {
					lastDelivered = at
				}
				mu.Unlock()
				done <- struct{}{}
			}
			d.mu.Unlock()
		}
		sel, count := map[string]any{"devices": names}, len(part)
		if o.activatePool {
			sel, count = map[string]any{"charging": true}, want
		}
		body, _ := json.Marshal(map[string]any{"id": ids[g], "priority": 50, "duration_s": o.activateDurS,
			"selection": sel,
			"level":     map[string]any{"current_a": o.activateLimitA},
			"stages":    []map[string]any{{"count": count, "spread_s": o.activateSpread}}})
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.apiURL, "/")+"/v1/activations", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", o.apiKey)
			t0 := time.Now()
			resp, err := client.Do(req)
			m.observe("activation.api_call", time.Since(t0))
			mu.Lock()
			if err != nil || resp.StatusCode != 201 {
				rep.APIErrors++
				if err == nil {
					log.Printf("activation: HTTP %d", resp.StatusCode)
				}
			} else {
				rep.APIAccepted++
			}
			mu.Unlock()
			if resp != nil {
				resp.Body.Close()
			}
		}(body)
	}
	wg.Wait()
	rep.SendSpanS = time.Since(start).Seconds()
	// until all wanted arrived, 60 s at most; 5 s without a new delivery
	// ends the wait too (the server may find fewer eligible than wanted)
	deadline := time.After(60 * time.Second)
	quiet := time.NewTimer(10 * time.Second)
	defer quiet.Stop()
	for got := 0; got < want; {
		select {
		case <-done:
			got++
			quiet.Reset(5 * time.Second)
		case <-quiet.C:
			got = want
		case <-deadline:
			got = want
		case <-ctx.Done():
			got = want
		}
	}
	mu.Lock()
	if !lastDelivered.IsZero() {
		rep.DeliverSpanS = lastDelivered.Sub(start).Seconds()
	}
	mu.Unlock()
	rep.PerDevice = statOf(m.samplesOf("activation.per_device"))
	log.Printf("activation: %d delivered, last %.2f s after the POST", rep.Delivered, rep.DeliverSpanS)
	time.Sleep(o.activateHold) // confirmations (and the server's resends) in, then the server's own count
	for _, id := range ids {
		req, _ := http.NewRequest("GET", strings.TrimRight(o.apiURL, "/")+"/v1/activations/"+id, nil)
		req.Header.Set("X-API-Key", o.apiKey)
		if resp, err := client.Do(req); err == nil {
			var a struct {
				Totals map[string]any `json:"totals"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&a)
			resp.Body.Close()
			log.Printf("activation %s on the server: %v", id, a.Totals)
		}
	}
	for _, d := range pick {
		d.mu.Lock()
		d.cmdAt, d.onDeliver = time.Time{}, nil
		d.mu.Unlock()
	}
	for _, id := range ids {
		req, _ := http.NewRequest("DELETE", strings.TrimRight(o.apiURL, "/")+"/v1/activations/"+id, nil)
		req.Header.Set("X-API-Key", o.apiKey)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	log.Printf("activation: ended (%d group commands)", k)
	return rep
}

// reconnect drops every connection at once (as a broker restart would) and
// brings the devices back at random within -reconnect-jitter.
func reconnect(ctx context.Context, o *options, devs []*device) *ReconnectReport {
	rep := &ReconnectReport{}
	var mu sync.Mutex
	var lastBack, lastPair time.Time
	drop := time.Now()
	var live []*device
	for _, d := range devs {
		if d.client != nil && d.client.IsConnected() {
			live = append(live, d)
		}
	}
	rep.Devices = len(live)
	log.Printf("reconnect: dropping %d connections", len(live))
	for _, d := range live {
		d.mu.Lock()
		d.droppedAt = drop
		d.onReconnect = func(d *device, at time.Time) {
			mu.Lock()
			rep.Reconnected++
			if at.After(lastBack) {
				lastBack = at
			}
			mu.Unlock()
		}
		d.onPairBack = func(d *device, at time.Time) {
			mu.Lock()
			rep.PairsBack++
			if at.After(lastPair) {
				lastPair = at
			}
			mu.Unlock()
			d.mu.Lock()
			d.onPairBack = nil // the first re-sent pair only
			d.mu.Unlock()
		}
		d.mu.Unlock()
		d.client.Disconnect(0)
	}
	var wg sync.WaitGroup
	for _, d := range live {
		wg.Add(1)
		go func(d *device) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(rand.Int63n(int64(o.reconnectJitter) + 1))):
			}
			for i := 0; i < 5 && ctx.Err() == nil; i++ {
				if d.connect() == nil {
					return
				}
				time.Sleep(time.Duration(1+rand.Intn(5)) * time.Second)
			}
		}(d)
	}
	wg.Wait()
	time.Sleep(5 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if !lastBack.IsZero() {
		rep.AllBackS = lastBack.Sub(drop).Seconds()
	}
	if !lastPair.IsZero() {
		rep.PairsBackS = lastPair.Sub(drop).Seconds()
	}
	log.Printf("reconnect: %d/%d back in %.1f s", rep.Reconnected, rep.Devices, rep.AllBackS)
	return rep
}

func (m *metrics) samplesOf(name string) []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]float64(nil), m.samples[name]...)
}

func sumPrefix(m *metrics, p string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, v := range m.counts {
		if strings.HasPrefix(k, p) {
			n += v
		}
	}
	return n
}

type mixEntry struct {
	p Profile
	w float64
}

func parseMix(s string) ([]mixEntry, error) {
	var out []mixEntry
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("profiles: %q, want name=share", part)
		}
		w, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			return nil, err
		}
		p := Profile(kv[0])
		if p != ProfileISO && p != ProfilePWM {
			return nil, fmt.Errorf("profiles: unknown %q (iso, pwm)", kv[0])
		}
		out = append(out, mixEntry{p, w})
	}
	return out, nil
}

func pickProfile(mix []mixEntry) Profile {
	total := 0.0
	for _, e := range mix {
		total += e.w
	}
	x := rand.Float64() * total
	for _, e := range mix {
		if x < e.w {
			return e.p
		}
		x -= e.w
	}
	return mix[len(mix)-1].p
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(time.Hour)
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	c := make(chan struct{})
	go func() { wg.Wait(); close(c) }()
	select {
	case <-c:
	case <-time.After(d):
		log.Print("some sessions did not close in time")
	}
}
