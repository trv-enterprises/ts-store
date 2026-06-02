// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package rollups implements persisted rollup aggregation: a background worker
// reads a high-frequency source store over closed time windows, aggregates
// each window, and writes one record per window into a second (target) store
// for cheap long-range queries.
package rollups

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/pkg/schema"
	"github.com/tviviano/ts-store/pkg/store"
)

var (
	ErrManagerClosed = errors.New("rollups manager is closed")
	ErrNotFound      = errors.New("rollup not found")
)

// Provider gives the rollups manager controlled access to the wider store
// service: opening other stores (the target), creating an auto-sized target,
// and deleting one on force_recreate. Implemented by the StoreService.
type Provider interface {
	// GetOrOpenStore returns an open store by name (opening it if needed).
	GetOrOpenStore(name string) (*store.Store, error)
	// CreateRollupTarget creates a schema-store target with the given config
	// and links its API keys to sourceStore. No-op error if it already exists.
	CreateRollupTarget(cfg store.Config, sourceStore string) (*store.Store, error)
	// DeleteStore deletes a store entirely (used on force_recreate resize).
	DeleteStore(name string) error
}

// Manager owns the rollup workers whose SOURCE is one store.
type Manager struct {
	mu sync.RWMutex

	source     *store.Store
	sourceName string
	provider   Provider

	workers map[string]*Worker // rollupID -> worker
	closed  bool
}

// NewManager constructs a manager. Call LoadAndStart to spin up persisted rollups.
func NewManager(src *store.Store, sourceName string, provider Provider) *Manager {
	return &Manager{
		source:     src,
		sourceName: sourceName,
		provider:   provider,
		workers:    make(map[string]*Worker),
	}
}

// LoadAndStart reads persisted rollup configs and starts a worker for each.
// Per-rollup failures are logged and skipped so one bad config doesn't block
// the rest.
func (m *Manager) LoadAndStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}

	cfgs, err := m.source.LoadRollupConfigs()
	if err != nil {
		return err
	}
	for _, rc := range cfgs.Rollups {
		w, err := m.buildWorker(rc)
		if err != nil {
			log.Printf("rollups %s/%s: build worker: %v", m.sourceName, rc.ID, err)
			continue
		}
		m.workers[rc.ID] = w
		w.Start()
	}
	return nil
}

// CreateRollupRequest is the wire shape for POST /api/stores/:store/rollups.
type CreateRollupRequest struct {
	Name          string  `json:"name,omitempty"`
	TargetStore   string  `json:"target_store,omitempty"`
	Window        string  `json:"window"`
	AggFields     string  `json:"agg_fields,omitempty"`
	AggDefault    string  `json:"agg_default,omitempty"`
	PollInterval  string  `json:"poll_interval,omitempty"`
	RestartPolicy string  `json:"restart_policy,omitempty"`
	Retention     string  `json:"retention,omitempty"`
	EdgeTolerance float64 `json:"edge_tolerance,omitempty"`
	ForceRecreate bool    `json:"force_recreate,omitempty"`
}

// CreateRollup validates the request, ensures the target store exists
// (auto-creating + sizing + schema-deriving it if absent), persists the config,
// and starts a worker.
func (m *Manager) CreateRollup(req CreateRollupRequest) (Status, error) {
	if req.Window == "" {
		return Status{}, fmt.Errorf("window is required")
	}
	if req.AggFields == "" && req.AggDefault == "" {
		return Status{}, fmt.Errorf("at least one of agg_fields or agg_default is required")
	}
	cw, _, err := canonicalWindow(req.Window)
	if err != nil {
		return Status{}, err
	}

	targetName := req.TargetStore
	if targetName == "" {
		targetName, err = derivedTargetName(m.sourceName, req.Window)
		if err != nil {
			return Status{}, err
		}
	} else if strings.ContainsAny(targetName, `/\`) || strings.Contains(targetName, "..") {
		return Status{}, fmt.Errorf("invalid target_store name %q", targetName)
	}
	if targetName == m.sourceName {
		return Status{}, fmt.Errorf("target_store must differ from the source store")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, ErrManagerClosed
	}

	target, err := m.ensureTarget(targetName, cw, req)
	if err != nil {
		return Status{}, err
	}

	rc := store.RollupConfig{
		ID:             uuid.New().String()[:8],
		Name:           req.Name,
		TargetStore:    targetName,
		WindowDuration: cw,
		AggFields:      req.AggFields,
		AggDefault:     req.AggDefault,
		PollInterval:   req.PollInterval,
		RestartPolicy:  req.RestartPolicy,
		Retention:      req.Retention,
		EdgeTolerance:  req.EdgeTolerance,
		CreatedAt:      time.Now().UTC(),
	}
	if err := m.source.AddRollupConfig(rc); err != nil {
		return Status{}, err
	}

	w, err := m.buildWorkerWithTarget(rc, target)
	if err != nil {
		_ = m.source.RemoveRollupConfig(rc.ID)
		return Status{}, err
	}
	m.workers[rc.ID] = w
	w.Start()
	return w.Status(), nil
}

// ensureTarget resolves the target store, auto-creating it (sized + schema'd)
// when absent. If it exists, it is reused only when it's a compatible rollup of
// this source; otherwise an error tells the caller to pick another name.
// force_recreate flushes/recreates an existing target before reuse.
func (m *Manager) ensureTarget(targetName, cw string, req CreateRollupRequest) (*store.Store, error) {
	derivedSchema, err := deriveTargetSchema(req.AggFields, req.AggDefault, sourceNumeric(m.source))
	if err != nil {
		return nil, fmt.Errorf("derive target schema: %w", err)
	}

	existing, openErr := m.provider.GetOrOpenStore(targetName)
	if openErr == nil && existing != nil {
		meta, _ := existing.ReadRollupMeta()
		if req.ForceRecreate {
			return m.recreateTarget(targetName, cw, req, derivedSchema)
		}
		if meta == nil || meta.RollupOf != m.sourceName {
			return nil, fmt.Errorf("store %q already exists and isn't a compatible rollup of %q; set target_store to a different name or pass force_recreate", targetName, m.sourceName)
		}
		// Existing compatible rollup target: ensure schema is set/extended
		// (append-only). Idempotent re-create lands here.
		if _, err := existing.SetSchema(derivedSchema); err != nil {
			return nil, fmt.Errorf("target %q schema incompatible (append-only): %w", targetName, err)
		}
		return existing, nil
	}

	// Auto-create a new, sized target.
	sz, err := deriveSizing(req.Retention, req.Window, req.AggFields, req.AggDefault, req.EdgeTolerance)
	if err != nil {
		return nil, err
	}
	cfg := targetConfig(targetName, "", sz) // Path filled by provider
	target, err := m.provider.CreateRollupTarget(cfg, m.sourceName)
	if err != nil {
		return nil, fmt.Errorf("create target %q: %w", targetName, err)
	}
	if _, err := target.SetSchema(derivedSchema); err != nil {
		return nil, fmt.Errorf("set target schema: %w", err)
	}
	if err := target.WriteRollupMeta(store.RollupMeta{
		RollupOf:   m.sourceName,
		Window:     cw,
		AggFields:  req.AggFields,
		AggDefault: req.AggDefault,
	}); err != nil {
		return nil, fmt.Errorf("write rollup meta: %w", err)
	}
	log.Printf("rollups %s: created target %q (P=%d, ~%s retention)", m.sourceName, targetName, sz.numPartitions, sz.actualRetention)
	return target, nil
}

// recreateTarget flushes (Reset) or fully recreates (Delete+Create) the target
// depending on whether sizing changed, then resets schema + sidecar.
func (m *Manager) recreateTarget(targetName, cw string, req CreateRollupRequest, derivedSchema *schema.Schema) (*store.Store, error) {
	// Always full delete+recreate on force_recreate for a clean slate and to
	// honor any sizing change (a live store can't resize).
	if err := m.provider.DeleteStore(targetName); err != nil {
		return nil, fmt.Errorf("recreate: delete %q: %w", targetName, err)
	}
	sz, err := deriveSizing(req.Retention, req.Window, req.AggFields, req.AggDefault, req.EdgeTolerance)
	if err != nil {
		return nil, err
	}
	cfg := targetConfig(targetName, "", sz)
	target, err := m.provider.CreateRollupTarget(cfg, m.sourceName)
	if err != nil {
		return nil, fmt.Errorf("recreate: create %q: %w", targetName, err)
	}
	if _, err := target.SetSchema(derivedSchema); err != nil {
		return nil, err
	}
	if err := target.WriteRollupMeta(store.RollupMeta{
		RollupOf: m.sourceName, Window: cw, AggFields: req.AggFields, AggDefault: req.AggDefault,
	}); err != nil {
		return nil, err
	}
	log.Printf("rollups %s: recreated target %q", m.sourceName, targetName)
	return target, nil
}

// ListRollups returns a status snapshot for every rollup managed here.
func (m *Manager) ListRollups() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, w.Status())
	}
	return out
}

// GetRollup returns the status of a single rollup by ID.
func (m *Manager) GetRollup(id string) (Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.workers[id]
	if !ok {
		return Status{}, ErrNotFound
	}
	return w.Status(), nil
}

// DeleteRollup stops the worker, removes the persisted config and cursor. The
// target store is left intact (its data may still be wanted).
func (m *Manager) DeleteRollup(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.workers[id]
	if !ok {
		return ErrNotFound
	}
	w.Stop()
	delete(m.workers, id)
	removeCursor(m.source, id)
	if err := m.source.RemoveRollupConfig(id); err != nil && err != store.ErrObjectNotFound {
		return err
	}
	return nil
}

// Stop stops all workers. Safe to call multiple times.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	for _, w := range m.workers {
		w.Stop()
	}
	m.workers = make(map[string]*Worker)
	return nil
}

// --- worker builders ---

func (m *Manager) buildWorker(rc store.RollupConfig) (*Worker, error) {
	target, err := m.provider.GetOrOpenStore(rc.TargetStore)
	if err != nil {
		return nil, fmt.Errorf("open target %q: %w", rc.TargetStore, err)
	}
	return m.buildWorkerWithTarget(rc, target)
}

func (m *Manager) buildWorkerWithTarget(rc store.RollupConfig, target *store.Store) (*Worker, error) {
	return NewWorker(Options{
		ID:             rc.ID,
		Name:           rc.Name,
		SourceName:     m.sourceName,
		TargetName:     rc.TargetStore,
		Source:         m.source,
		Target:         target,
		WindowDuration: rc.WindowDuration,
		AggFields:      rc.AggFields,
		AggDefault:     rc.AggDefault,
		PollInterval:   rc.PollInterval,
		RestartPolicy:  rc.RestartPolicy,
		CursorPath:     cursorPathFor(m.source, rc.ID),
		CreatedAt:      rc.CreatedAt,
	})
}

func cursorPathFor(st *store.Store, id string) string {
	return filepath.Join(st.StorePath(), "rollup_"+id+".cursor")
}

func removeCursor(st *store.Store, id string) {
	_ = os.Remove(cursorPathFor(st, id))
}

// sourceNumeric returns the source store's field-name -> is-numeric map, used
// to type derived aggregate fields and to know which fields a default applies
// to. Nil for non-schema sources (callers handle that as "no numeric info").
func sourceNumeric(src *store.Store) map[string]bool {
	return aggregation.BuildNumericMap(src.GetSchemaSet())
}
