// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

// Package tunnel defines the device protocol: the MQTT envelope of the V2G
// tunnel between a device and the engine, and the device's other messages.
//
//	dev/<device>/v2g/up    device -> engine   one EV request (or connection event)
//	dev/<device>/v2g/down  engine -> device   the response
//	dev/<device>/trace     device -> server   timing of the delivered response
//	dev/<device>/cl/push   engine -> device   Charging Loop pair without a request
//	dev/<device>/ctrl      engine -> device   commands (ttl_renew)
//	dev/<device>/config    engine -> device   settings (retained)
//	dev/<device>/status    device -> server   heartbeat (retained) + LWT
//	dev/<device>/cp        device -> server   CP state (retained), on every change
//	dev/<device>/cp/set    engine -> device   CP command: mode, or a restart pulse
//	dev/<device>/power/set engine -> device   basic-mode current limit (PWM), 0 = none
//	dev/<device>/telemetry/energy device -> server  phase currents, energy (F9)
//
// JSON (EXI as base64). Times: microseconds since the epoch.
package tunnel

import "strings"

const Version = 1

// Root is the first level of the device protocol's topics: dev/<device>/...
// The same for every server built on this protocol and every device.
const Root = "dev"

// Up is one message from the device.
type Up struct {
	V    int    `json:"v"`
	Conn string `json:"conn"`          // TCP connection id (EV <-> device), unique per device
	Seq  uint32 `json:"seq"`           // per connection, starting at 1
	PT   uint16 `json:"pt,omitempty"`  // V2GTP payload type, 0x8001 = EXI
	EXI  []byte `json:"exi,omitempty"` // payload without the V2GTP header
	// Wire size of the request as received from the EV (V2GTP header + EXI,
	// TCP/IP overhead not included).
	WireBytes int `json:"wire_bytes,omitempty"`
	// Sampled: a Charging Loop request the device already answered from
	// its cache, forwarded at most once per T seconds as a liveness sign.
	// The engine journals it and does not answer.
	Sampled bool `json:"sampled,omitempty"`
	// Event instead of a message: "open" (EV connected) or "closed".
	Event  string `json:"event,omitempty"`
	Reason string `json:"reason,omitempty"` // with "closed": why the device closed it
	TLS    string `json:"tls,omitempty"`    // with "open": TLS version and cipher to the EV, empty = plain TCP
	TEVRx  int64  `json:"t_ev_rx"`
	TUpTx  int64  `json:"t_up_tx"`
	// Plug: the plug-in id ("<boot>-<n>") the device was in when it sent this
	// (ADR-003); "" from a device before 0.3.2.
	Plug string `json:"plug,omitempty"`
	// LastTS (ISO 15118-20): the highest header time stamp, microseconds,
	// the device wrote into an answer from its cache on this connection; the
	// server's next answer is higher (V2G20-1537, ADR-005). 0: none yet.
	LastTS uint64 `json:"last_ts,omitempty"`
}

// Down is the engine's answer.
type Down struct {
	V     int    `json:"v"`
	Conn  string `json:"conn"`
	Seq   uint32 `json:"seq"` // the request's seq
	PT    uint16 `json:"pt"`
	EXI   []byte `json:"exi,omitempty"`
	Close bool   `json:"close,omitempty"` // close the TCP connection after sending
	// CL: new Charging Loop pair, sent with the response that changed it, so
	// the pair is installed before the EV's next loop request.
	CL     *CachePair `json:"cl,omitempty"`
	TSrvRx int64      `json:"t_srv_rx"`
	TSrvTx int64      `json:"t_srv_tx"`
}

// Trace reports when the device got the response and passed it to the EV.
type Trace struct {
	V       int    `json:"v"`
	Conn    string `json:"conn"`
	Seq     uint32 `json:"seq"`
	TDownRx int64  `json:"t_down_rx"`
	TEVTx   int64  `json:"t_ev_tx"`
	// FromCache: the device answered from its Charging Loop cache (or from
	// the local termination scenario: Term); the request went nowhere else.
	FromCache bool   `json:"from_cache,omitempty"`
	Term      bool   `json:"term,omitempty"`
	TEVRx     int64  `json:"t_ev_rx,omitempty"`
	ReqBytes  int    `json:"req_bytes,omitempty"`
	ResBytes  int    `json:"res_bytes,omitempty"`
	Note      string `json:"note,omitempty"`
}

// CachePair: the EV's ChargingStatusReq bytes, the response to give for
// them, and the local termination scenario (StopCharging, PowerDeliveryRes,
// SessionStopRes). Ver: newer replaces older; TTLS also renews the TTL.
//
// ISO 15118-20 (Kind "prefix", ADR-005): the loop request changes every time
// (time stamp, the EV's values), so the device compares only the first
// ReqBits bits of Req (message type and session ID) and the payload type PT;
// before it sends Res or a Term answer it writes the current Unix time as five
// 7-bit EXI groups (40 bits) at bit ResTS / TermTS[i], and sends Term[i] with
// payload type TermPT[i]. A device that does not know Kind compares all bytes:
// it never hits, every loop request goes to the server (no wrong answer).
type CachePair struct {
	Ver  int64    `json:"ver"`
	Req  []byte   `json:"req"`
	Res  []byte   `json:"res"`
	Term [][]byte `json:"term,omitempty"`
	TTLS int      `json:"ttl_s"`
	// ISO 15118-20 only (Kind "prefix").
	Kind    string   `json:"kind,omitempty"`
	PT      uint16   `json:"pt,omitempty"`
	ReqBits int      `json:"req_bits,omitempty"`
	ResTS   int      `json:"res_ts_bit,omitempty"`
	TermPT  []uint16 `json:"term_pt,omitempty"`
	TermTS  []int    `json:"term_ts_bit,omitempty"`
	// TS: the time stamp (microseconds) the answers were encoded with; the
	// device writes max(its clock, TS + 1, its last + 1) each time.
	TS uint64 `json:"ts,omitempty"`
	// CommandID: the operator command this answer carries (its limit), echoed
	// back in the Ack; 0 when the pair follows from the station settings only.
	CommandID int64 `json:"command_id,omitempty"`
}

// Push carries a pair without a request (after the device reconnects, or a
// limit change between loop requests).
type Push struct {
	V    int        `json:"v"`
	Conn string     `json:"conn"`
	CL   *CachePair `json:"cl"`
}

// Ctrl is a command; "ttl_renew" only renews the TTL.
type Ctrl struct {
	V    int    `json:"v"`
	Cmd  string `json:"cmd"`
	TTLS int    `json:"ttl_s,omitempty"`
	TSrv int64  `json:"t_srv"`
}

// Config is the device's settings (retained, versioned).
type Config struct {
	V            int   `json:"v"`
	Ver          int64 `json:"ver"`           // monotonic: the device ignores older ones
	Rev          int   `json:"rev"`           // device_configs version (0 = server defaults); echoed in status`
	SampleS      int   `json:"loop_sample_s"` // T: forward a cache hit at most once per T s; 0 = all
	TTLS         int   `json:"ttl_s"`         // default TTL
	HeartbeatS   int   `json:"heartbeat_s"`   // status period; also the loop_active window
	TermBackupS  int   `json:"term_backup_s"` // termination scenario must finish within this
	Trace        bool  `json:"trace"`         // per-frame timing (diagnostics)
	CacheEnabled bool  `json:"cache_enabled"` // off = every request to the server (comparison runs)
	// DigitalMode: 5 % to the EV (HLC); off = transit, basic charging on the station PWM.
	DigitalMode bool `json:"digital_mode"`
	// HLCWaitS: digital mode, EV plugged in but no V2G connection in this
	// time: the device goes to the station PWM side (basic). 0 = wait forever.
	HLCWaitS int `json:"hlc_wait_s"`
	// TelemetryS: metering report period while current flows.
	TelemetryS int `json:"telemetry_s"`
	// Cloud only (the device ignores them): a DIN-only EV whose SoC was read
	// at plug-in is read again when it stops charging by itself (C -> B on the
	// PWM side for SoCFinalWaitS): final SoC, the battery capacity from the two.
	SoCFinal      bool `json:"soc_final"`
	SoCFinalWaitS int  `json:"soc_final_wait_s"`
}

// CPState is the device's CP report (fields used by the engine).
type CPState struct {
	Mode    string         `json:"mode"` // requested: digital | transit | basic | X1 | F | E
	Out     string         `json:"out"`  // applied: mask | transit | X1 | F | E
	Duty    float64        `json:"duty"` // mask duty, % (5 digital; limit / 0.6 basic)
	LimitA  *float64       `json:"limit_a"`
	EV      string         `json:"ev"` // EV state A..E
	Station map[string]any `json:"station"`
	Power   *bool          `json:"power"`
	Event   string         `json:"event,omitempty"` // plugged | unplugged
	Note    string         `json:"note,omitempty"`
	Slac    string         `json:"slac,omitempty"`   // UNMATCHED | MATCHING | MATCHED (the device's SLAC)
	EVMac   string         `json:"ev_mac,omitempty"` // the EV's PLC MAC from SLAC
	// ADR-003: the plug-in this report belongs to ("<boot>-<n>": boot id of
	// the device's software start, plug-in number since then; "" without
	// an EV) and, with "unplugged", how long the plug-in lasted by the
	// device's monotonic clock. T: the device's wall clock, diagnostics only.
	Plug      string `json:"plug,omitempty"`
	PluggedMs *int64 `json:"plugged_ms,omitempty"`
	T         int64  `json:"t,omitempty"`
}

// Telemetry: phase currents (clamps), power and energy at the nominal voltage.
type Telemetry struct {
	Seq    int64     `json:"seq"`  // per boot: the newest report wins (ADR-003)
	Boot   string    `json:"boot"` // boot id; "" before 0.3.2 (then arrival order)
	Plug   string    `json:"plug,omitempty"`
	T      int64     `json:"t"` // device wall clock, diagnostics only
	IA     []float64 `json:"i_a"`
	PTotal float64   `json:"p_total_w"`
	EWh    float64   `json:"e_wh"`
}

// CPSet: {"mode": "digital"|"transit"} or {"cmd": "restart", "via": "X1"|"F", "hold_s": 3}.
type CPSet struct {
	V     int     `json:"v"`
	Mode  string  `json:"mode,omitempty"`
	Cmd   string  `json:"cmd,omitempty"`
	Via   string  `json:"via,omitempty"`
	HoldS float64 `json:"hold_s,omitempty"`
}

// Status is the device heartbeat (fields used by the engine).
type Status struct {
	Online bool   `json:"online"`
	Event  string `json:"event,omitempty"` // "connect" on the first status after (re)connecting
}

// DeviceFromTopic returns <device> of "dev/<device>/...".
func DeviceFromTopic(topic string) string {
	p := strings.Split(topic, "/")
	if len(p) < 3 || p[0] != Root {
		return ""
	}
	return p[1]
}

func DownTopic(device string) string   { return Root + "/" + device + "/v2g/down" }
func PushTopic(device string) string   { return Root + "/" + device + "/cl/push" }
func CtrlTopic(device string) string   { return Root + "/" + device + "/ctrl" }
func ConfigTopic(device string) string { return Root + "/" + device + "/config" }
func CPSetTopic(device string) string  { return Root + "/" + device + "/cp/set" }
func TelemetryTopic() string           { return Root + "/+/telemetry/energy" }

// Ack: dev/<id>/cmd/ack (QoS 1) — the device took a command over.
//
//	kind "pair":  a Charging Loop pair (v2g/down or cl/push); result
//	              "installed" (the EV gets the new answer from its next loop
//	              request), "stale" (an older version than the one held),
//	              "rejected" (a local termination is running).
//	kind "power": power/set; result "applied" (the CP is on the station's PWM
//	              side, the mask is set), "held" (kept until the CP gets
//	              there: a digital session runs), "cleared", "stale".
//
// The engine counts the first "installed" / "applied" per command id; repeats
// and reordered acks change nothing (ADR-001, rule 4).
type Ack struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	Ver       int64  `json:"ver"`
	CommandID int64  `json:"command_id,omitempty"`
	Conn      string `json:"conn,omitempty"`
	Result    string `json:"result"`
	Plug      string `json:"plug,omitempty"` // ADR-003
	T         int64  `json:"t"`              // device wall clock, diagnostics only (the reaction time is the server's)
}

func AckTopic() string { return Root + "/+/cmd/ack" }

func PowerSetTopic(device string) string {
	return Root + "/" + device + "/power/set"
}

// PowerSet: the current limit in basic mode (no HLC: the device masks the
// station PWM to LimitA / 0.6 %). Held by the device for DurationS or until
// its TTL ends without renewal; LimitA 0 clears it.
type PowerSet struct {
	V         int     `json:"v"`
	Ver       int64   `json:"ver"`
	CommandID int64   `json:"command_id,omitempty"`
	LimitA    float64 `json:"limit_a"`
	DurationS int     `json:"duration_s,omitempty"`
	TTLS      int     `json:"ttl_s"`
	TSrv      int64   `json:"t_srv"`
}
