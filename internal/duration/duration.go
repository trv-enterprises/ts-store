// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package duration provides extended duration parsing beyond Go's standard library.
package duration

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses a duration string like "30s", "15m", "2h", "7d", "1w",
// "3mo", "1y". Extends Go's time.ParseDuration to support days (d), weeks (w),
// months (mo, = 30 days), and years (y, = 365 days). Months/years are nominal
// (calendar-agnostic) — used for retention sizing, not exact calendar math.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	// Extended units. Check longer suffixes first ("mo" before "m").
	type unit struct {
		suffix string
		scale  time.Duration
	}
	for _, u := range []unit{
		{"mo", 30 * 24 * time.Hour},
		{"y", 365 * 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	} {
		if strings.HasSuffix(s, u.suffix) {
			numStr := strings.TrimSuffix(s, u.suffix)
			num, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration: %s", s)
			}
			d := time.Duration(num * float64(u.scale))
			if d < 0 {
				return 0, fmt.Errorf("duration must not be negative: %s", s)
			}
			return d, nil
		}
	}

	// Fall back to standard Go duration parsing (s, m, h, etc.)
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must not be negative: %s", s)
	}
	return d, nil
}
