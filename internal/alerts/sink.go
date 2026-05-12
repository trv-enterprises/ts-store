// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package alerts implements webhook, WS, and MQTT alert workers that
// evaluate rules against a store's data stream and dispatch matching
// records through a configured Sink.
package alerts

import "github.com/tviviano/ts-store/internal/notify"

// Sink dispatches an alert to a transport. Implementations may queue
// internally; Send returning nil does not guarantee delivery completed.
// Close is called once on shutdown and must release any held resources.
type Sink interface {
	Send(alert notify.Alert) error
	Close() error
}
