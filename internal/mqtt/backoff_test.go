// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package mqtt

import (
	"testing"
	"time"
)

// TestNextRetryDelay (issue #65): the reconnect delay must keep growing
// for post-connect failures — a remote that accepts the dial but fails
// every write used to reset the backoff to 1s on each connect.
func TestNextRetryDelay(t *testing.T) {
	max := 60 * time.Second

	// No progress: exponential growth, capped.
	d := time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		d = nextRetryDelay(d, max, false)
		if d != w {
			t.Errorf("step %d: delay = %v, want %v", i, d, w)
		}
	}
	if got := nextRetryDelay(59*time.Second, max, false); got != max {
		t.Errorf("cap: delay = %v, want %v", got, max)
	}
	if got := nextRetryDelay(max, max, false); got != max {
		t.Errorf("at cap: delay = %v, want %v", got, max)
	}

	// Progress resets to 1s regardless of how high the delay grew.
	if got := nextRetryDelay(32*time.Second, max, true); got != time.Second {
		t.Errorf("progress: delay = %v, want 1s", got)
	}
}
