-- SPDX-License-Identifier: Apache-2.0
-- Copyright Contributors to the project. See NOTICE.

-- A plug-in opened by the projector without its session.started (issue #15,
-- 26.11.2026). An event of a plug-in id the projector has not seen started
-- opens the plug-in's summary at the time of that event; implicit_start
-- marks it. A session.started of the same id coming later joins the summary
-- (its start moves back, the mark is cleared). The device's plug-in still
-- open at that moment ends with end_reason 'superseded'.

ALTER TABLE public.session_summaries ADD COLUMN implicit_start boolean DEFAULT false NOT NULL;
