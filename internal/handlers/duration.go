// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses a duration string like "30s", "15m", "2h", "7d", "1w".
// Extends Go's time.ParseDuration to support days (d) and weeks (w).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	// Check for our extended units (days, weeks)
	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		num, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		return time.Duration(num * float64(24*time.Hour)), nil
	}

	if strings.HasSuffix(s, "w") {
		numStr := strings.TrimSuffix(s, "w")
		num, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		return time.Duration(num * float64(7*24*time.Hour)), nil
	}

	// Fall back to standard Go duration parsing (s, m, h, etc.)
	return time.ParseDuration(s)
}
