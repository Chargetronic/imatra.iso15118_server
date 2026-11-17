// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.

package main

import (
	"context"
	"time"
)

// retentionLoop keeps the operator event feed (domain_events) to
// EVENTS_RETENTION_DAYS (30) and the history (telemetry, the twin journal
// events, the V2G journal messages) to HISTORY_RETENTION_DAYS (30); the
// leader does it, once an hour. 0 keeps everything.
func (e *engine) retentionLoop() {
	days := int(envF("EVENTS_RETENTION_DAYS", 30))
	hist := int(envF("HISTORY_RETENTION_DAYS", 30))
	if days <= 0 && hist <= 0 {
		return
	}
	for {
		if e.coord.isLeader() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if days > 0 {
				if n, err := e.db.PurgeDomainEvents(ctx, days); err != nil {
					e.log.Warn("purge feed events", "err", err)
				} else if n > 0 {
					e.log.Info("feed events purged", "rows", n, "older_than_days", days)
				}
			}
			if hist > 0 {
				for _, t := range []string{"telemetry", "events", "messages"} {
					if n, err := e.db.PurgeHistory(ctx, t, hist, 5000); err != nil {
						e.log.Warn("purge history", "table", t, "err", err)
					} else if n > 0 {
						e.log.Info("history purged", "table", t, "rows", n, "older_than_days", hist)
					}
				}
			}
			cancel()
		}
		time.Sleep(time.Hour)
	}
}
