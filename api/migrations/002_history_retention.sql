-- SPDX-License-Identifier: Apache-2.0
-- Copyright Contributors to the project. See NOTICE.

-- History tables kept to a retention period, and the attributes query indexed
-- (load test 16-17.11.2026).
--
-- 1. The projector recalculates a device's attributes after each plug-in
--    (reaction to limits over 30 days). limits had no index for it: the
--    partial index limits_active covers only limits not cancelled, so every
--    recalculation read the whole table, which grows with every activation.
-- 2. The engine's leader deletes telemetry, the twin journal (events) and the
--    V2G journal (messages) older than HISTORY_RETENTION_DAYS (30). Before,
--    nothing deleted them. BRIN indexes on the time columns: a few pages,
--    almost free to maintain on insert, enough to find the old rows.

CREATE INDEX limits_device_acked ON public.limits USING btree (device_id, acked_at) WHERE (acked_at IS NOT NULL);

CREATE INDEX telemetry_time ON public.telemetry USING brin (server_ts);

CREATE INDEX messages_time ON public.messages USING brin (server_ts);
