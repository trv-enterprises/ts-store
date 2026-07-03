// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"testing"
	"time"
)

// Regression tests for issue #11: key management (tsstore key revoke /
// regenerate) runs as a separate process writing keys.json directly, so a
// long-running server's Manager must pick up on-disk changes. Two Manager
// instances over the same base path simulate the two processes.

// settle gives the filesystem a distinct mtime for the next write on
// filesystems with coarse timestamp resolution.
func settle() { time.Sleep(10 * time.Millisecond) }

func TestRevocationVisibleToRunningManager(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	key, entry, err := server.Generate("sensors", "test key")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Warm the server's cache
	if _, err := server.Validate("sensors", key); err != nil {
		t.Fatalf("Validate before revoke: %v", err)
	}

	settle()
	if err := cli.Revoke("sensors", entry.ID); err != nil {
		t.Fatalf("Revoke via second manager: %v", err)
	}

	// The running server must reject the revoked key without a restart
	if _, err := server.Validate("sensors", key); err == nil {
		t.Fatal("revoked key still accepted by running manager (stale cache)")
	}
}

func TestRegeneratedKeyVisibleToRunningManager(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	oldKey, _, err := server.Generate("sensors", "old")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := server.Validate("sensors", oldKey); err != nil {
		t.Fatalf("Validate old key: %v", err)
	}

	settle()
	newKey, _, err := cli.Regenerate("sensors", "rotated")
	if err != nil {
		t.Fatalf("Regenerate via second manager: %v", err)
	}

	// New key works immediately on the running server...
	if _, err := server.Validate("sensors", newKey); err != nil {
		t.Fatalf("regenerated key rejected by running manager (stale cache): %v", err)
	}
	// ...and the rotated-out key no longer does
	if _, err := server.Validate("sensors", oldKey); err == nil {
		t.Fatal("rotated-out key still accepted by running manager")
	}
}

func TestValidateConcurrentWithRotation(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	key, _, err := server.Generate("sensors", "seed")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Hammer Validate while the "CLI" rotates keys; run with -race this
	// verifies the cache is not mutated under a read lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			if _, _, err := cli.Regenerate("sensors", "rotate"); err != nil {
				t.Errorf("Regenerate: %v", err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			server.Validate("sensors", key) // outcome varies; must not race or panic
		}
	}
}
