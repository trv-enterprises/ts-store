// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"time"

	"github.com/tviviano/ts-store/internal/duration"
)

// ParseDuration parses a duration string like "30s", "15m", "2h", "7d", "1w".
// Extends Go's time.ParseDuration to support days (d) and weeks (w).
func ParseDuration(s string) (time.Duration, error) {
	return duration.ParseDuration(s)
}
