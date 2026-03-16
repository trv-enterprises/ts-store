// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package service

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
)

// newTestService creates a StoreService backed by a temp directory.
// The caller should defer svc.CloseAll().
func newTestService(t *testing.T) *StoreService {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Store: config.StoreConfig{
			BasePath:       tmpDir,
			DataBlockSize:  4096,
			IndexBlockSize: 4096,
			NumBlocks:      64,
		},
	}

	keyManager := apikey.NewManager(tmpDir)
	return NewStoreService(cfg, keyManager)
}

// ---------- Service lifecycle ----------

func TestNewStoreService(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	if svc == nil {
		t.Fatal("NewStoreService returned nil")
	}
}

func TestCreateStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	resp, err := svc.Create(&CreateStoreRequest{Name: "test-create"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if resp.Name != "test-create" {
		t.Errorf("expected name 'test-create', got %q", resp.Name)
	}
	if resp.APIKey == "" {
		t.Error("expected non-empty API key")
	}
	if resp.KeyID == "" {
		t.Error("expected non-empty key ID")
	}
}

func TestCreateDuplicateStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{Name: "dup"})
	if err != nil {
		t.Fatalf("first Create failed: %v", err)
	}

	_, err = svc.Create(&CreateStoreRequest{Name: "dup"})
	if err == nil {
		t.Fatal("expected error creating duplicate store, got nil")
	}
}

func TestGetStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{Name: "get-me"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	st, err := svc.Get("get-me")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if st == nil {
		t.Fatal("Get returned nil store")
	}
}

func TestGetNonexistentStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Get("does-not-exist")
	if err == nil {
		t.Fatal("expected error getting nonexistent store, got nil")
	}
	if err != ErrStoreNotOpen {
		t.Errorf("expected ErrStoreNotOpen, got %v", err)
	}
}

func TestGetOrOpen(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	// Create and then close a store
	_, err := svc.Create(&CreateStoreRequest{Name: "reopen"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := svc.Close("reopen"); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify it is closed
	_, err = svc.Get("reopen")
	if err != ErrStoreNotOpen {
		t.Fatalf("expected ErrStoreNotOpen after close, got %v", err)
	}

	// GetOrOpen should reopen it
	st, err := svc.GetOrOpen("reopen")
	if err != nil {
		t.Fatalf("GetOrOpen failed: %v", err)
	}
	if st == nil {
		t.Fatal("GetOrOpen returned nil")
	}
}

// ---------- Store management ----------

func TestListOpen(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, _ = svc.Create(&CreateStoreRequest{Name: "open-a"})
	_, _ = svc.Create(&CreateStoreRequest{Name: "open-b"})

	names := svc.ListOpen()
	sort.Strings(names)

	if len(names) != 2 {
		t.Fatalf("expected 2 open stores, got %d", len(names))
	}
	if names[0] != "open-a" || names[1] != "open-b" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestListAll(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Store: config.StoreConfig{
			BasePath:       tmpDir,
			DataBlockSize:  4096,
			IndexBlockSize: 4096,
			NumBlocks:      64,
		},
	}

	keyManager := apikey.NewManager(tmpDir)
	svc1 := NewStoreService(cfg, keyManager)

	_, _ = svc1.Create(&CreateStoreRequest{Name: "persist-a"})
	_, _ = svc1.Create(&CreateStoreRequest{Name: "persist-b"})
	svc1.CloseAll()

	// New service pointing at the same directory should discover both on disk
	svc2 := NewStoreService(cfg, keyManager)
	defer svc2.CloseAll()

	all := svc2.ListAll()
	sort.Strings(all)

	if len(all) != 2 {
		t.Fatalf("expected 2 stores on disk, got %d: %v", len(all), all)
	}
	if all[0] != "persist-a" || all[1] != "persist-b" {
		t.Errorf("unexpected names: %v", all)
	}
}

func TestCloseStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, _ = svc.Create(&CreateStoreRequest{Name: "close-me"})

	if err := svc.Close("close-me"); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	names := svc.ListOpen()
	if len(names) != 0 {
		t.Errorf("expected 0 open stores after close, got %d", len(names))
	}
}

func TestCloseAll(t *testing.T) {
	svc := newTestService(t)

	_, _ = svc.Create(&CreateStoreRequest{Name: "all-a"})
	_, _ = svc.Create(&CreateStoreRequest{Name: "all-b"})
	_, _ = svc.Create(&CreateStoreRequest{Name: "all-c"})

	if err := svc.CloseAll(); err != nil {
		t.Fatalf("CloseAll failed: %v", err)
	}

	names := svc.ListOpen()
	if len(names) != 0 {
		t.Errorf("expected 0 open stores after CloseAll, got %d", len(names))
	}
}

// ---------- Operations ----------

func TestStoreStats(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{Name: "stats-store"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	stats, err := svc.Stats("stats-store")
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if stats == nil {
		t.Fatal("Stats returned nil")
	}
	if stats.DataBlockSize != 4096 {
		t.Errorf("expected DataBlockSize 4096, got %d", stats.DataBlockSize)
	}
	if stats.DataType != "json" {
		t.Errorf("expected DataType 'json', got %q", stats.DataType)
	}
}

func TestResetStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{Name: "reset-store"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Write some data
	st, _ := svc.Get("reset-store")
	_, err = st.PutObject(1000000000, []byte(`{"value":1}`))
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// Reset
	if err := svc.Reset("reset-store"); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Verify stats show empty store
	stats, _ := svc.Stats("reset-store")
	if stats.ActiveBlocks != 0 && stats.OldestTimestamp != 0 {
		t.Errorf("expected empty store after reset, got ActiveBlocks=%d OldestTimestamp=%d",
			stats.ActiveBlocks, stats.OldestTimestamp)
	}
}

func TestDeleteStore(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{Name: "delete-me"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	storePath := filepath.Join(svc.cfg.Store.BasePath, "delete-me")
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		t.Fatal("store directory should exist before delete")
	}

	if err := svc.Delete("delete-me"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should not be in open list
	names := svc.ListOpen()
	for _, n := range names {
		if n == "delete-me" {
			t.Error("deleted store still in open list")
		}
	}

	// Directory should be gone
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Error("store directory still exists after delete")
	}
}

// ---------- V2 store creation ----------

func TestCreateV2Store(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	// Default storage type is V2
	resp, err := svc.Create(&CreateStoreRequest{Name: "v2-default"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if resp.Name != "v2-default" {
		t.Errorf("unexpected name %q", resp.Name)
	}

	st, _ := svc.Get("v2-default")
	if !st.IsV2() {
		t.Error("expected V2 store by default")
	}
}

func TestCreateV1Store(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	resp, err := svc.Create(&CreateStoreRequest{
		Name:        "v1-store",
		StorageType: "v1",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if resp.Name != "v1-store" {
		t.Errorf("unexpected name %q", resp.Name)
	}

	st, _ := svc.Get("v1-store")
	if st.IsV2() {
		t.Error("expected V1 store when storage_type=v1")
	}
}

func TestCreateStoreWithPartitionOptions(t *testing.T) {
	svc := newTestService(t)
	defer svc.CloseAll()

	_, err := svc.Create(&CreateStoreRequest{
		Name:          "custom-parts",
		NumPartitions: 4,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	st, _ := svc.Get("custom-parts")
	if !st.IsV2() {
		t.Fatal("expected V2 store")
	}

	cfg := st.Config()
	if cfg.NumPartitions != 4 {
		t.Errorf("expected 4 partitions, got %d", cfg.NumPartitions)
	}
}
