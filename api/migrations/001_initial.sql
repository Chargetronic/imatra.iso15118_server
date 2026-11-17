-- SPDX-License-Identifier: Apache-2.0
-- Copyright Contributors to the project. See NOTICE.

-- Schema of one deployment of the cloud ISO 15118 server, first release.
--
-- Devices and their settings (devices, device_configs), the engines' journal
-- of the digital twin (sessions, messages, events, telemetry), operator
-- limits and activations (limits, activations, activation_stages,
-- activation_targets), the event feed (domain_events, relay_state) and the
-- operator tables built from it by the projector (device_live,
-- session_summaries, device_attributes, projector_state, projector_orphans),
-- operator keys and their audit (api_keys, audit_log).
--
-- Device registry of the service layer (devices.bind_state ... registry_at):
-- the layer publishes, retained, one message per device on registry/dev/<id>
-- in the operator's broker: bound / unbound / revoked, device type, coarse
-- location. The leader engine applies a message only if its binding_ver is
-- greater than the one stored (repeats and late messages change nothing).
--   bind_state   NULL: not in the registry (admitted by its certificate alone)
--   bound_tz     the user's time zone, as the layer gives it
--   registry_at  when the last registry message was applied
-- Coordinates come from the registry only.
--
-- The running sessions themselves live in Valkey (ADR-002), not here.
-- Later changes come as 002_*.sql, ... (applied in order by the API at start,
-- bookkeeping in schema_migrations).

CREATE TABLE public.activation_stages (
    activation_id text NOT NULL,
    stage smallint NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    count integer,
    share numeric(4,3),
    spread_ms integer DEFAULT 0 NOT NULL,
    state text DEFAULT 'scheduled'::text NOT NULL,
    selected integer,
    started_at timestamp with time zone,
    dispatched_at timestamp with time zone,
    reported_at timestamp with time zone
);

CREATE TABLE public.activation_targets (
    activation_id text NOT NULL,
    device_id text NOT NULL,
    stage smallint NOT NULL,
    command_id bigint NOT NULL,
    limit_a numeric(5,1) NOT NULL,
    power_w_before numeric(8,0),
    limit_w numeric(8,0),
    send_at timestamp with time zone NOT NULL,
    queued_at timestamp with time zone,
    resent boolean DEFAULT false NOT NULL
);

CREATE TABLE public.activations (
    activation_id text NOT NULL,
    key_name text NOT NULL,
    priority smallint NOT NULL,
    selection jsonb NOT NULL,
    level jsonb NOT NULL,
    ends_at timestamp with time zone NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    request jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ended_at timestamp with time zone,
    CONSTRAINT activations_priority_check CHECK (((priority >= 1) AND (priority <= 100)))
);

CREATE TABLE public.api_keys (
    key_id integer NOT NULL,
    name text NOT NULL,
    prefix text NOT NULL,
    hash bytea NOT NULL,
    rights text[] NOT NULL,
    source text DEFAULT 'cli'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone,
    CONSTRAINT api_keys_rights_check CHECK ((rights <@ ARRAY['read'::text, 'control'::text, 'debug'::text]))
);

CREATE SEQUENCE public.api_keys_key_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.api_keys_key_id_seq OWNED BY public.api_keys.key_id;

CREATE TABLE public.audit_log (
    id bigint NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    key_name text NOT NULL,
    action text NOT NULL,
    device_id text,
    command_id bigint,
    request jsonb,
    status smallint NOT NULL
);

CREATE SEQUENCE public.audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.audit_log_id_seq OWNED BY public.audit_log.id;

CREATE TABLE public.device_attributes (
    device_id text NOT NULL,
    firmware text,
    connectivity text,
    attrs jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.device_configs (
    device_id text NOT NULL,
    version integer NOT NULL,
    config jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by text,
    note text
);

CREATE TABLE public.device_live (
    device_id text NOT NULL,
    online boolean,
    cp text,
    charging boolean DEFAULT false NOT NULL,
    control_mode text,
    plugged_since timestamp with time zone,
    plug_dseq bigint,
    v2g_session_id uuid,
    protocol text,
    limit_a numeric(5,1),
    limit_source text,
    limit_until timestamp with time zone,
    command_id bigint,
    limit_confirmed_at timestamp with time zone,
    power_w numeric(8,0),
    power_quality text,
    e_amount_wh numeric(9,0),
    soc smallint,
    departure_time_s integer,
    online_time timestamp with time zone,
    cp_time timestamp with time zone,
    power_time timestamp with time zone,
    as_of timestamp with time zone NOT NULL,
    last_seq bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.devices (
    device_id text NOT NULL,
    description text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone,
    online boolean DEFAULT false NOT NULL,
    status jsonb,
    debug_journal boolean DEFAULT false NOT NULL,
    event_seq bigint DEFAULT 0 NOT NULL,
    lat numeric(6,3),
    lon numeric(6,3),
    location_at timestamp with time zone,
    blocked_at timestamp with time zone,
    blocked_reason text,
    blocked_by text,
    rules jsonb DEFAULT '{}'::jsonb NOT NULL,
    bind_state text,
    binding_ver bigint DEFAULT 0 NOT NULL,
    device_type text,
    bound_at timestamp with time zone,
    unbound_at timestamp with time zone,
    bound_tz text,
    registry_at timestamp with time zone,
    CONSTRAINT devices_bind_state_check CHECK ((bind_state = ANY (ARRAY['bound'::text, 'unbound'::text, 'revoked'::text]))),
    CONSTRAINT devices_lat_check CHECK (((lat >= ('-90'::integer)::numeric) AND (lat <= (90)::numeric))),
    CONSTRAINT devices_lon_check CHECK (((lon >= ('-180'::integer)::numeric) AND (lon <= (180)::numeric)))
);

CREATE TABLE public.domain_events (
    seq bigint NOT NULL,
    id text NOT NULL,
    type text NOT NULL,
    schema_ver smallint DEFAULT 1 NOT NULL,
    device_id text,
    session_id uuid,
    dseq bigint,
    "time" timestamp with time zone NOT NULL,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.domain_events_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.domain_events_seq_seq OWNED BY public.domain_events.seq;

CREATE TABLE public.events (
    id bigint NOT NULL,
    device_id text,
    session_id uuid,
    kind text NOT NULL,
    data jsonb,
    device_ts timestamp with time zone,
    server_ts timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.events_id_seq OWNED BY public.events.id;

CREATE TABLE public.limits (
    command_id bigint NOT NULL,
    device_id text NOT NULL,
    limit_a numeric(5,1) NOT NULL,
    starts_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    source text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    cancelled_at timestamp with time zone,
    applied_at timestamp with time zone,
    delivered_at timestamp with time zone,
    acked_at timestamp with time zone,
    ack_via text,
    reaction_ms integer,
    priority smallint DEFAULT 0 NOT NULL,
    activation_id text,
    CONSTRAINT limits_limit_a_check CHECK ((limit_a >= (6)::numeric))
);

CREATE SEQUENCE public.limits_command_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.limits_command_id_seq OWNED BY public.limits.command_id;

CREATE TABLE public.messages (
    id bigint NOT NULL,
    session_id uuid,
    seq integer,
    direction text NOT NULL,
    source text NOT NULL,
    msg_type text,
    exi bytea,
    decoded jsonb,
    payload_bytes integer,
    wire_bytes integer,
    device_ts timestamp with time zone,
    server_ts timestamp with time zone DEFAULT now() NOT NULL,
    conn_id text,
    t_ev_rx bigint,
    t_up_tx bigint,
    t_srv_rx bigint,
    t_srv_tx bigint,
    t_down_rx bigint,
    t_ev_tx bigint,
    pair_id bigint,
    CONSTRAINT messages_direction_check CHECK ((direction = ANY (ARRAY['ev_to_server'::text, 'server_to_ev'::text])))
);

CREATE SEQUENCE public.messages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.messages_id_seq OWNED BY public.messages.id;

CREATE TABLE public.projector_orphans (
    device_id text NOT NULL,
    seq bigint NOT NULL,
    "time" timestamp with time zone NOT NULL,
    plug text
);

CREATE TABLE public.projector_state (
    name text NOT NULL,
    cursor bigint DEFAULT 0 NOT NULL,
    as_of timestamp with time zone,
    lag_ms integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.relay_state (
    name text NOT NULL,
    last_id text,
    run_id text,
    gap_mark text,
    lag_ms integer DEFAULT 0 NOT NULL,
    backlog bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.session_summaries (
    summary_id text NOT NULL,
    device_id text NOT NULL,
    start_dseq bigint NOT NULL,
    end_dseq bigint,
    started_at timestamp with time zone NOT NULL,
    ended_at timestamp with time zone,
    mode text,
    v2g_sessions smallint DEFAULT 0 NOT NULL,
    has_eamount boolean DEFAULT false NOT NULL,
    has_soc boolean DEFAULT false NOT NULL,
    e_amount_wh numeric(9,0),
    soc_start smallint,
    departure_time_s integer,
    charging_s integer DEFAULT 0 NOT NULL,
    limits_applied smallint DEFAULT 0 NOT NULL,
    energy_wh numeric(10,2),
    energy_quality text,
    duration_s integer,
    end_reason text,
    data_gap boolean DEFAULT false NOT NULL,
    charging_since timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    plug_id text
);

CREATE TABLE public.sessions (
    session_id uuid NOT NULL,
    device_id text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    ended_at timestamp with time zone,
    mode text,
    end_reason text,
    evcc_id text,
    ev_soc_start numeric,
    ev_params_start jsonb,
    device_config_version integer,
    energy_wh numeric,
    conn_id text,
    protocol text,
    v2g_session_id text,
    phase text,
    engine_state jsonb,
    ev_data jsonb,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    hot_ver bigint
);

CREATE TABLE public.telemetry (
    id bigint NOT NULL,
    device_id text NOT NULL,
    session_id uuid,
    device_ts timestamp with time zone,
    server_ts timestamp with time zone DEFAULT now() NOT NULL,
    i_a real[] NOT NULL,
    p_w real NOT NULL,
    e_wh double precision NOT NULL,
    data jsonb
);

CREATE SEQUENCE public.telemetry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.telemetry_id_seq OWNED BY public.telemetry.id;

ALTER TABLE ONLY public.api_keys ALTER COLUMN key_id SET DEFAULT nextval('public.api_keys_key_id_seq'::regclass);

ALTER TABLE ONLY public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq'::regclass);

ALTER TABLE ONLY public.domain_events ALTER COLUMN seq SET DEFAULT nextval('public.domain_events_seq_seq'::regclass);

ALTER TABLE ONLY public.events ALTER COLUMN id SET DEFAULT nextval('public.events_id_seq'::regclass);

ALTER TABLE ONLY public.limits ALTER COLUMN command_id SET DEFAULT nextval('public.limits_command_id_seq'::regclass);

ALTER TABLE ONLY public.messages ALTER COLUMN id SET DEFAULT nextval('public.messages_id_seq'::regclass);

ALTER TABLE ONLY public.telemetry ALTER COLUMN id SET DEFAULT nextval('public.telemetry_id_seq'::regclass);

ALTER TABLE ONLY public.activation_stages
    ADD CONSTRAINT activation_stages_pkey PRIMARY KEY (activation_id, stage);

ALTER TABLE ONLY public.activation_targets
    ADD CONSTRAINT activation_targets_pkey PRIMARY KEY (activation_id, device_id);

ALTER TABLE ONLY public.activations
    ADD CONSTRAINT activations_pkey PRIMARY KEY (activation_id);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_hash_key UNIQUE (hash);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_name_key UNIQUE (name);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (key_id);

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.device_attributes
    ADD CONSTRAINT device_attributes_pkey PRIMARY KEY (device_id);

ALTER TABLE ONLY public.device_configs
    ADD CONSTRAINT device_configs_pkey PRIMARY KEY (device_id, version);

ALTER TABLE ONLY public.device_live
    ADD CONSTRAINT device_live_pkey PRIMARY KEY (device_id);

ALTER TABLE ONLY public.devices
    ADD CONSTRAINT devices_pkey PRIMARY KEY (device_id);

ALTER TABLE ONLY public.domain_events
    ADD CONSTRAINT domain_events_id_key UNIQUE (id);

ALTER TABLE ONLY public.domain_events
    ADD CONSTRAINT domain_events_pkey PRIMARY KEY (seq);

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.limits
    ADD CONSTRAINT limits_pkey PRIMARY KEY (command_id);

ALTER TABLE ONLY public.messages
    ADD CONSTRAINT messages_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.projector_orphans
    ADD CONSTRAINT projector_orphans_pkey PRIMARY KEY (seq);

ALTER TABLE ONLY public.projector_state
    ADD CONSTRAINT projector_state_pkey PRIMARY KEY (name);

ALTER TABLE ONLY public.relay_state
    ADD CONSTRAINT relay_state_pkey PRIMARY KEY (name);

ALTER TABLE ONLY public.session_summaries
    ADD CONSTRAINT session_summaries_pkey PRIMARY KEY (summary_id);

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (session_id);

ALTER TABLE ONLY public.telemetry
    ADD CONSTRAINT telemetry_pkey PRIMARY KEY (id);

CREATE INDEX activation_stages_due ON public.activation_stages USING btree (starts_at) WHERE (state = ANY (ARRAY['scheduled'::text, 'dispatching'::text]));

CREATE INDEX activation_targets_queue ON public.activation_targets USING btree (send_at) WHERE (queued_at IS NULL);

CREATE INDEX audit_log_at ON public.audit_log USING btree (at DESC);

CREATE INDEX audit_log_device ON public.audit_log USING btree (device_id, at DESC);

CREATE INDEX device_live_charging ON public.device_live USING btree (charging) WHERE charging;

CREATE INDEX devices_blocked ON public.devices USING btree (device_id) WHERE (blocked_at IS NOT NULL);

CREATE INDEX devices_bind_state ON public.devices USING btree (bind_state);

CREATE INDEX domain_events_device ON public.domain_events USING btree (device_id, dseq);

CREATE INDEX domain_events_recorded ON public.domain_events USING btree (recorded_at);

CREATE INDEX domain_events_type ON public.domain_events USING btree (type, seq);

CREATE INDEX events_device_kind ON public.events USING btree (device_id, kind, server_ts DESC);

CREATE INDEX events_time ON public.events USING btree (server_ts DESC);

CREATE INDEX limits_activation ON public.limits USING btree (activation_id) WHERE (activation_id IS NOT NULL);

CREATE INDEX limits_active ON public.limits USING btree (device_id, priority DESC, command_id DESC) WHERE (cancelled_at IS NULL);

CREATE INDEX messages_seq ON public.messages USING btree (session_id, seq);

CREATE INDEX messages_session ON public.messages USING btree (session_id, id);

CREATE INDEX projector_orphans_device ON public.projector_orphans USING btree (device_id, "time");

CREATE INDEX session_summaries_device ON public.session_summaries USING btree (device_id, start_dseq DESC);

CREATE INDEX session_summaries_plug ON public.session_summaries USING btree (device_id, plug_id) WHERE (plug_id IS NOT NULL);

CREATE INDEX session_summaries_time ON public.session_summaries USING btree (started_at DESC);

CREATE UNIQUE INDEX sessions_conn ON public.sessions USING btree (device_id, conn_id) WHERE (conn_id IS NOT NULL);

CREATE INDEX sessions_open ON public.sessions USING btree (device_id, started_at DESC) WHERE (ended_at IS NULL);

CREATE INDEX sessions_started ON public.sessions USING btree (started_at DESC);

CREATE INDEX telemetry_device_ts ON public.telemetry USING btree (device_id, server_ts DESC);

ALTER TABLE ONLY public.activation_stages
    ADD CONSTRAINT activation_stages_activation_id_fkey FOREIGN KEY (activation_id) REFERENCES public.activations(activation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.activation_targets
    ADD CONSTRAINT activation_targets_activation_id_fkey FOREIGN KEY (activation_id) REFERENCES public.activations(activation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.device_configs
    ADD CONSTRAINT device_configs_device_id_fkey FOREIGN KEY (device_id) REFERENCES public.devices(device_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_device_id_fkey FOREIGN KEY (device_id) REFERENCES public.devices(device_id);

ALTER TABLE ONLY public.limits
    ADD CONSTRAINT limits_device_id_fkey FOREIGN KEY (device_id) REFERENCES public.devices(device_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_device_id_fkey FOREIGN KEY (device_id) REFERENCES public.devices(device_id);

ALTER TABLE ONLY public.telemetry
    ADD CONSTRAINT telemetry_device_id_fkey FOREIGN KEY (device_id) REFERENCES public.devices(device_id) ON DELETE CASCADE;

-- Functions (after the tables their bodies read)

CREATE FUNCTION public.stream_id_max(a text, b text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
  SELECT CASE WHEN (split_part(a, '-', 1)::bigint, split_part(a, '-', 2)::bigint)
                >= (split_part(b, '-', 1)::bigint, split_part(b, '-', 2)::bigint) THEN a ELSE b END
$$;

CREATE FUNCTION public.telemetry_energy(dev text, t0 timestamp with time zone, t1 timestamp with time zone) RETURNS double precision
    LANGUAGE sql STABLE
    AS $$
  SELECT COALESCE(sum(GREATEST(d, 0)), 0) FROM (
    SELECT e_wh - lag(e_wh) OVER (ORDER BY server_ts, id) AS d
    FROM telemetry WHERE device_id = dev AND server_ts >= t0 AND server_ts <= t1) x
$$;
