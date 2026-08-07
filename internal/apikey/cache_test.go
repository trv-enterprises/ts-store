// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package apikey

import (
	"testing"
	"time"
)

// Regression tests for issue #11: key management (tsstore key revoke /
// create) runs as a separate process writing the registry directly, so a
// long-running server's Manager must pick up on-disk changes. Two Manager
// instances over the same base path simulate the two processes.
//
// The mechanism changed in #138 (per-store keys.json -> central
// registry) but the contract did not: a revoked key stops working
// without a restart. These tests are the contract.

// settle gives the filesystem a distinct mtime for the next write on
// filesystems with coarse timestamp resolution.
func settle() { time.Sleep(10 * time.Millisecond) }

// fullGrant is the all-access-on-one-store grant a store's own key gets.
func fullGrant(store string) []Grant {
	return []Grant{{Stores: store, Access: append([]Access(nil), AllAccess...)}}
}

func TestRevocationVisibleToRunningManager(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	key, entry, err := server.Create("test key", fullGrant("sensors"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Warm the server's cache
	if _, err := server.Authorize("sensors", key, AccessRead); err != nil {
		t.Fatalf("Authorize before revoke: %v", err)
	}

	settle()
	if err := cli.Revoke(entry.ID); err != nil {
		t.Fatalf("Revoke via second manager: %v", err)
	}

	// The running server must reject the revoked key without a restart
	if _, err := server.Authorize("sensors", key, AccessRead); err == nil {
		t.Fatal("revoked key still accepted by running manager (stale cache)")
	}
}

func TestRotatedKeyVisibleToRunningManager(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	oldKey, oldEntry, err := server.Create("old", fullGrant("sensors"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := server.Authorize("sensors", oldKey, AccessRead); err != nil {
		t.Fatalf("Authorize old key: %v", err)
	}

	settle()
	// Rotation is now revoke + create rather than a single Regenerate.
	if err := cli.Revoke(oldEntry.ID); err != nil {
		t.Fatalf("Revoke via second manager: %v", err)
	}
	newKey, _, err := cli.Create("rotated", fullGrant("sensors"))
	if err != nil {
		t.Fatalf("Create via second manager: %v", err)
	}

	// New key works immediately on the running server...
	if _, err := server.Authorize("sensors", newKey, AccessRead); err != nil {
		t.Fatalf("rotated-in key rejected by running manager (stale cache): %v", err)
	}
	// ...and the rotated-out key no longer does
	if _, err := server.Authorize("sensors", oldKey, AccessRead); err == nil {
		t.Fatal("rotated-out key still accepted by running manager")
	}
}

func TestAuthorizeConcurrentWithRotation(t *testing.T) {
	base := t.TempDir()

	server := NewManager(base)
	cli := NewManager(base)

	key, _, err := server.Create("seed", fullGrant("sensors"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Hammer Authorize while the "CLI" rotates keys. Under -race this
	// verifies the read-lock fast path never observes a cache another
	// goroutine is mutating, and that readers holding a *RegistryKey
	// into the cached registry are never written through.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			k, entry, err := cli.Create("rotate", fullGrant("sensors"))
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			_ = k
			if err := cli.Revoke(entry.ID); err != nil {
				t.Errorf("Revoke: %v", err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			// Outcome varies; must not race or panic.
			_, _ = server.Authorize("sensors", key, AccessRead)
			_, _ = server.List()
		}
	}
}

// TestCacheFastPathSeesExternalWrite is the specific hazard the read-lock
// fast path introduces: it serves from cache without reloading, so it
// must still stat the file and fall through when it changed.
func TestCacheFastPathSeesExternalWrite(t *testing.T) {
	base := t.TempDir()
	server := NewManager(base)
	cli := NewManager(base)

	key, entry, err := server.Create("k", fullGrant("sensors"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Warm the cache twice so the fast path is definitely populated.
	for i := 0; i < 2; i++ {
		if _, err := server.Authorize("sensors", key, AccessRead); err != nil {
			t.Fatalf("warm Authorize: %v", err)
		}
	}

	settle()
	if err := cli.Revoke(entry.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, err := server.Authorize("sensors", key, AccessRead); err == nil {
		t.Fatal("fast path served a revoked key from a stale cache")
	}
}
