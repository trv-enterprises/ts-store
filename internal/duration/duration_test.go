// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package duration

import (
	"testing"
	"time"
)

func TestParseDurationUnits(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"2h", 2 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"3mo", 3 * 30 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"0s", 0},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Errorf("ParseDuration(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "12", "d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) expected error, got nil", in)
		}
	}
}

// Regression test for issue #10: negative durations must be rejected — they
// flow into poll intervals, cooldowns, retention, and rollup windows, where
// every caller treats them as magnitudes.
func TestParseDurationRejectsNegative(t *testing.T) {
	for _, in := range []string{"-1s", "-5m", "-1d", "-0.5w", "-2mo", "-1y"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) expected error for negative duration, got nil", in)
		}
	}
}
