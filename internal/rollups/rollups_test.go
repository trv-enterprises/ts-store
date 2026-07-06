// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rollups

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tviviano/ts-store/pkg/schema"
	"github.com/tviviano/ts-store/pkg/store"
)

// fakeProvider creates/opens stores under a temp base path. Stand-in for the
// StoreService in unit tests.
type fakeProvider struct {
	basePath string
	open     map[string]*store.Store
}

func newFakeProvider(basePath string) *fakeProvider {
	return &fakeProvider{basePath: basePath, open: map[string]*store.Store{}}
}

func (p *fakeProvider) GetOrOpenStore(name string) (*store.Store, error) {
	if st, ok := p.open[name]; ok {
		return st, nil
	}
	st, err := store.Open(p.basePath, name)
	if err != nil {
		return nil, err
	}
	p.open[name] = st
	return st, nil
}

func (p *fakeProvider) CreateRollupTarget(cfg store.Config, sourceStore string) (*store.Store, error) {
	cfg.Path = p.basePath
	st, err := store.Create(cfg)
	if err != nil {
		return nil, err
	}
	p.open[cfg.Name] = st
	return st, nil
}

func (p *fakeProvider) DeleteStore(name string) error {
	if st, ok := p.open[name]; ok {
		delete(p.open, name)
		return st.Delete()
	}
	return store.DeleteStore(p.basePath, name)
}

// newSourceStore makes a schema source store with a single numeric "temp" field.
func newSourceStore(t *testing.T, basePath, name string) *store.Store {
	t.Helper()
	cfg := store.DefaultConfig()
	cfg.Name = name
	cfg.Path = basePath
	cfg.DataType = store.DataTypeSchema
	st, err := store.Create(cfg)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := st.SetSchema(&schema.Schema{Fields: []schema.Field{
		{Index: 1, Name: "temp", Type: schema.FieldTypeFloat64},
	}}); err != nil {
		t.Fatalf("set source schema: %v", err)
	}
	return st
}

// putTemp writes a {"temp": v} record at ts.
func putTemp(t *testing.T, st *store.Store, ts int64, v float64) {
	t.Helper()
	compact, err := st.ValidateAndCompact([]byte(`{"temp":` + ftoa(v) + `}`))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := st.PutObject(ts, compact); err != nil {
		t.Fatalf("put @%d: %v", ts, err)
	}
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestCanonicalWindow(t *testing.T) {
	cases := map[string]string{
		"60m":   "1h",
		"1h":    "1h",
		"3600s": "1h",
		"24h":   "1d",
		"7d":    "1w",
		"90m":   "90m",
		"30s":   "30s",
	}
	for in, want := range cases {
		got, _, err := canonicalWindow(in)
		if err != nil {
			t.Errorf("canonicalWindow(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("canonicalWindow(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDerivedTargetName(t *testing.T) {
	name, err := derivedTargetName("sensors", "60m")
	if err != nil {
		t.Fatal(err)
	}
	if name != "sensors-1h" {
		t.Errorf("derived name = %q, want sensors-1h", name)
	}
}

func TestDeriveSizing(t *testing.T) {
	// 1y hourly, 10% edge tolerance.
	sz, err := deriveSizing("1y", "1h", 2, 0.10)
	if err != nil {
		t.Fatalf("deriveSizing: %v", err)
	}
	if sz.numPartitions < 2 || sz.numPartitions > 16 {
		t.Errorf("partitions out of range: %d", sz.numPartitions)
	}
	// Rounds up: actual retention must be >= requested (1 year).
	year := 365 * 24 * time.Hour
	if sz.actualRetention < year {
		t.Errorf("actual retention %s < requested 1y", sz.actualRetention)
	}
	if sz.totalSize <= 0 {
		t.Error("totalSize must be positive")
	}
}

func TestDeriveTargetSchema(t *testing.T) {
	srcNumeric := map[string]bool{"temp": true, "name": false}

	// Single function -> field name as-is; window_count appended.
	sch, err := deriveTargetSchema("temp:avg", "", srcNumeric)
	if err != nil {
		t.Fatal(err)
	}
	names := fieldNames(sch)
	if !contains(names, "temp") || !contains(names, windowCountField) {
		t.Errorf("single-func schema fields = %v", names)
	}

	// Multi-function -> suffixed names.
	sch2, err := deriveTargetSchema("temp:avg+max", "", srcNumeric)
	if err != nil {
		t.Fatal(err)
	}
	n2 := fieldNames(sch2)
	if !contains(n2, "temp_avg") || !contains(n2, "temp_max") {
		t.Errorf("multi-func schema fields = %v", n2)
	}
}

func fieldNames(s *schema.Schema) []string {
	var out []string
	for _, f := range s.Fields {
		out = append(out, f.Name)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestRollupOnce_TrailingClosedWindows is the core behavior test: minute
// windows, several full + one open, only closed windows written, correct avg
// and window_count, labeled at window end.
func TestRollupOnce_TrailingClosedWindows(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "sensors")
	defer src.Close()

	minute := int64(time.Minute)
	// Choose a base aligned to a minute boundary well in the past.
	now := time.Now().UnixNano()
	start := ((now-100*minute)/minute)*minute // aligned, ~100m ago

	// Window A [start, start+1m): two samples 10 and 20 -> avg 15, count 2.
	putTemp(t, src, start+1, 10)
	putTemp(t, src, start+2, 20)
	// Window B [start+1m, start+2m): one sample 30 -> avg 30, count 1.
	putTemp(t, src, start+minute+1, 30)
	// Window C (empty) — skipped.
	// Window D [start+3m, ...): one sample 40.
	putTemp(t, src, start+3*minute+1, 40)

	// Build the target + worker directly (no background loop) so we control
	// exactly when rollupOnce runs.
	sz, err := deriveSizing("1d", "1m", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	target, err := provider.CreateRollupTarget(targetConfig("sensors-1m", "", sz), "sensors")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	tsch, _ := deriveTargetSchema("temp:avg", "", map[string]bool{"temp": true})
	if _, err := target.SetSchema(tsch); err != nil {
		t.Fatalf("set target schema: %v", err)
	}

	w, err := NewWorker(Options{
		ID: "t1", SourceName: "sensors", TargetName: "sensors-1m",
		Source: src, Target: target,
		WindowDuration: "1m", AggFields: "temp:avg", RestartPolicy: "now",
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if err := w.rollupOnce(); err != nil {
		t.Fatalf("rollupOnce: %v", err)
	}

	// Read all target records.
	handles, err := target.GetOldestObjects(0)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	type row struct {
		ts    int64
		temp  float64
		count float64
	}
	var rows []row
	for _, h := range handles {
		data, err := target.GetObject(h)
		if err != nil {
			t.Fatal(err)
		}
		full, err := target.ExpandData(data, 0)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]interface{}
		json.Unmarshal(full, &m)
		rows = append(rows, row{
			ts:    h.Timestamp,
			temp:  m["temp"].(float64),
			count: m[windowCountField].(float64),
		})
	}

	// Windows A, B, D written (C empty -> skipped) = 3 rows.
	if len(rows) != 3 {
		t.Fatalf("expected 3 rollup rows, got %d: %+v", len(rows), rows)
	}
	// Window A: labeled at end = start+1m, avg 15, count 2.
	if rows[0].ts != start+minute {
		t.Errorf("row A ts = %d, want %d (window END)", rows[0].ts, start+minute)
	}
	if rows[0].temp != 15 || rows[0].count != 2 {
		t.Errorf("row A = avg %v count %v, want 15/2", rows[0].temp, rows[0].count)
	}
	// Window B: end start+2m, avg 30, count 1.
	if rows[1].ts != start+2*minute || rows[1].temp != 30 || rows[1].count != 1 {
		t.Errorf("row B = ts %d avg %v count %v", rows[1].ts, rows[1].temp, rows[1].count)
	}
	// Window D: end start+4m, avg 40.
	if rows[2].ts != start+4*minute || rows[2].temp != 40 {
		t.Errorf("row D = ts %d avg %v, want %d/40", rows[2].ts, rows[2].temp, start+4*minute)
	}
}

// TestRollupOnce_Idempotent verifies a second pass with no new closed window
// writes nothing (no duplicate / no ErrTimestampOutOfOrder).
func TestRollupOnce_Idempotent(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "s")
	defer src.Close()

	minute := int64(time.Minute)
	now := time.Now().UnixNano()
	start := ((now - 50*minute) / minute) * minute
	putTemp(t, src, start+1, 5)
	putTemp(t, src, start+minute+1, 7)

	mgr := NewManager(src, "s", provider)
	defer mgr.Stop()
	st, err := mgr.CreateRollup(CreateRollupRequest{Window: "1m", AggFields: "temp:avg", Retention: "1d"})
	if err != nil {
		t.Fatalf("CreateRollup: %v", err)
	}
	mgr.mu.RLock()
	w := mgr.workers[st.ID]
	mgr.mu.RUnlock()
	w.Stop()

	if err := w.rollupOnce(); err != nil {
		t.Fatalf("first rollupOnce: %v", err)
	}
	target, _ := provider.GetOrOpenStore("s-1m")
	h1, _ := target.GetOldestObjects(0)
	n1 := len(h1)

	// Second pass: no new closed windows -> no new writes.
	if err := w.rollupOnce(); err != nil {
		t.Fatalf("second rollupOnce: %v", err)
	}
	h2, _ := target.GetOldestObjects(0)
	if len(h2) != n1 {
		t.Errorf("idempotency: rows changed from %d to %d on second pass", n1, len(h2))
	}
}

// TestTargetAutoCreatedAsRollupSchemaStore checks the target is created, is a
// schema store, and carries the rollup sidecar.
func TestTargetAutoCreatedAsRollupSchemaStore(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "src")
	defer src.Close()

	mgr := NewManager(src, "src", provider)
	defer mgr.Stop()
	if _, err := mgr.CreateRollup(CreateRollupRequest{Window: "1h", AggFields: "temp:avg+max", Retention: "30d"}); err != nil {
		t.Fatalf("CreateRollup: %v", err)
	}

	target, err := provider.GetOrOpenStore("src-1h")
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if target.DataType() != store.DataTypeSchema {
		t.Errorf("target data type = %v, want schema", target.DataType())
	}
	meta, err := target.ReadRollupMeta()
	if err != nil || meta == nil {
		t.Fatalf("rollup meta missing: %v", err)
	}
	if meta.RollupOf != "src" || meta.Window != "1h" {
		t.Errorf("rollup meta = %+v, want rollup_of=src window=1h", meta)
	}
	// Sidecar should exist on disk too.
	if _, err := store.ReadRollupMetaAt(filepath.Join(base, "src-1h")); err != nil {
		t.Errorf("sidecar read: %v", err)
	}
}

// TestCreateRollup_RejectsIncompatibleExistingTarget ensures pointing at an
// existing unrelated store errors rather than clobbering it.
func TestCreateRollup_RejectsIncompatibleExistingTarget(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "src")
	defer src.Close()

	// Pre-create an unrelated store named like the derived target.
	other := store.DefaultConfig()
	other.Name = "src-1h"
	other.Path = base
	other.DataType = store.DataTypeJSON
	st, err := store.Create(other)
	if err != nil {
		t.Fatal(err)
	}
	provider.open["src-1h"] = st

	mgr := NewManager(src, "src", provider)
	defer mgr.Stop()
	_, err = mgr.CreateRollup(CreateRollupRequest{Window: "1h", AggFields: "temp:avg", Retention: "30d"})
	if err == nil {
		t.Fatal("expected error pointing rollup at incompatible existing store")
	}
}

// Regression tests for issue #14: README-API promises identical re-creates
// are idempotent and parameter changes without force_recreate are rejected.
// Before the fix every POST appended a new config and started a second
// worker racing on the same target windows.

func TestCreateRollupIdempotent(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "idem")
	defer src.Close()

	mgr := NewManager(src, "idem", provider)
	defer mgr.Stop()

	req := CreateRollupRequest{Window: "1m", AggFields: "temp:avg", Retention: "1d"}
	st1, err := mgr.CreateRollup(req)
	if err != nil {
		t.Fatalf("first CreateRollup: %v", err)
	}
	st2, err := mgr.CreateRollup(req)
	if err != nil {
		t.Fatalf("identical CreateRollup should be idempotent, got error: %v", err)
	}
	if st2.ID != st1.ID {
		t.Errorf("identical re-create returned a new rollup: %s vs %s", st2.ID, st1.ID)
	}

	cfgs, err := src.LoadRollupConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs.Rollups) != 1 {
		t.Errorf("expected 1 persisted config, got %d", len(cfgs.Rollups))
	}
	mgr.mu.RLock()
	n := len(mgr.workers)
	mgr.mu.RUnlock()
	if n != 1 {
		t.Errorf("expected 1 running worker, got %d", n)
	}
}

func TestCreateRollupSpecChangeRejected(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "reject")
	defer src.Close()

	mgr := NewManager(src, "reject", provider)
	defer mgr.Stop()

	if _, err := mgr.CreateRollup(CreateRollupRequest{Window: "1m", AggFields: "temp:avg", Retention: "1d"}); err != nil {
		t.Fatalf("first CreateRollup: %v", err)
	}

	// Same derived target (window unchanged), different agg spec
	_, err := mgr.CreateRollup(CreateRollupRequest{Window: "1m", AggFields: "temp:avg+max", Retention: "1d"})
	if err == nil {
		t.Fatal("parameter-changing POST without force_recreate was accepted")
	}
	if !strings.Contains(err.Error(), "force_recreate") {
		t.Errorf("rejection should mention force_recreate, got: %v", err)
	}

	cfgs, _ := src.LoadRollupConfigs()
	if len(cfgs.Rollups) != 1 {
		t.Errorf("expected 1 persisted config after rejection, got %d", len(cfgs.Rollups))
	}
}

func TestCreateRollupForceRecreateReplaces(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "force")
	defer src.Close()

	mgr := NewManager(src, "force", provider)
	defer mgr.Stop()

	st1, err := mgr.CreateRollup(CreateRollupRequest{Window: "1m", AggFields: "temp:avg", Retention: "1d"})
	if err != nil {
		t.Fatalf("first CreateRollup: %v", err)
	}

	st2, err := mgr.CreateRollup(CreateRollupRequest{Window: "1m", AggFields: "temp:avg+max", Retention: "1d", ForceRecreate: true})
	if err != nil {
		t.Fatalf("force_recreate CreateRollup: %v", err)
	}
	if st2.ID == st1.ID {
		t.Errorf("force_recreate should retire the old rollup and mint a new one")
	}

	cfgs, _ := src.LoadRollupConfigs()
	if len(cfgs.Rollups) != 1 {
		t.Errorf("expected exactly 1 persisted config after force_recreate, got %d", len(cfgs.Rollups))
	}
	mgr.mu.RLock()
	_, oldAlive := mgr.workers[st1.ID]
	_, newAlive := mgr.workers[st2.ID]
	n := len(mgr.workers)
	mgr.mu.RUnlock()
	if oldAlive || !newAlive || n != 1 {
		t.Errorf("worker map wrong after force_recreate: old=%v new=%v total=%d", oldAlive, newAlive, n)
	}
}

// Regression test for issue #25: target sizing used a blind estimate
// (8 fields per default function) instead of the derived schema it had just
// computed, oversizing the fixed-footprint target several-fold. For a
// 1-field source with default "min,max,avg" the schema is 4 fields
// (3 aggregates + window_count); the old estimate assumed 25.
func TestTargetSizedFromDerivedSchema(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "sized")
	defer src.Close()

	mgr := NewManager(src, "sized", provider)
	defer mgr.Stop()

	if _, err := mgr.CreateRollup(CreateRollupRequest{
		Window: "1m", AggDefault: "min,max,avg", Retention: "1d",
	}); err != nil {
		t.Fatalf("CreateRollup: %v", err)
	}

	target, err := provider.GetOrOpenStore("sized-1m")
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	cfg := target.Config()
	footprint := int64(cfg.BlocksPerPartition()) * int64(cfg.NumPartitions) * int64(cfg.DataBlockSize)

	// 1440 records/day at ~4 fields x 16 bytes each fits comfortably under
	// 300KB; the old 25-field estimate produced ~655KB.
	if footprint > 300_000 {
		t.Errorf("target footprint %d bytes — sized from an estimate, not the derived schema", footprint)
	}
	if footprint <= 0 {
		t.Errorf("nonsensical footprint %d", footprint)
	}
}

// Regression test for issue #17: a source store gaining a field after the
// worker derived its config permanently stalled the rollup — the aggregated
// row carried the new field, the target's ValidateAndCompact rejected it,
// and the cursor never advanced past the poison window.
func TestRollupSurvivesSourceSchemaGrowth(t *testing.T) {
	base := t.TempDir()
	provider := newFakeProvider(base)
	src := newSourceStore(t, base, "drift") // schema: {temp}
	defer src.Close()

	minute := int64(time.Minute)
	now := time.Now().UnixNano()
	start := ((now - 100*minute) / minute) * minute

	// Window A: temp only
	putTemp(t, src, start+1, 10)

	sz, err := deriveSizing("1d", "1m", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	target, err := provider.CreateRollupTarget(targetConfig("drift-1m", "", sz), "drift")
	if err != nil {
		t.Fatal(err)
	}
	tsch, err := deriveTargetSchema("", "avg", map[string]bool{"temp": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.SetSchema(tsch); err != nil {
		t.Fatal(err)
	}

	w, err := NewWorker(Options{
		ID: "d1", SourceName: "drift", TargetName: "drift-1m",
		Source: src, Target: target,
		WindowDuration: "1m", AggDefault: "avg", RestartPolicy: "now",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.rollupOnce(); err != nil {
		t.Fatalf("first rollupOnce: %v", err)
	}

	// The source gains a field (append-only schema evolution)...
	if _, err := src.SetSchema(&schema.Schema{Fields: []schema.Field{
		{Index: 1, Name: "temp", Type: schema.FieldTypeFloat64},
		{Index: 2, Name: "hum", Type: schema.FieldTypeFloat64},
	}}); err != nil {
		t.Fatalf("grow source schema: %v", err)
	}
	// ...and window B carries the new field.
	compact, err := src.ValidateAndCompact([]byte(`{"temp": 20, "hum": 55}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.PutObject(start+minute+1, compact); err != nil {
		t.Fatal(err)
	}

	// The first pass swept past window B while it was still empty; rewind
	// the cursor to just before it, as a resume-policy worker would after
	// a restart, so this pass re-reads the window that now has data.
	w.mu.Lock()
	w.lastWindowEnd = start + minute
	w.mu.Unlock()

	if err := w.rollupOnce(); err != nil {
		t.Fatalf("rollupOnce after source schema growth: %v (rollup would stall forever)", err)
	}

	handles, err := target.GetOldestObjects(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 2 {
		t.Fatalf("expected 2 rollup rows after schema growth, got %d", len(handles))
	}

	data, err := target.GetObject(handles[1])
	if err != nil {
		t.Fatal(err)
	}
	full, err := target.ExpandData(data, 0)
	if err != nil {
		t.Fatalf("expand row B: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(full, &m); err != nil {
		t.Fatal(err)
	}
	if m["temp"] != 20.0 || m["hum"] != 55.0 {
		t.Errorf("row B = %v, want temp=20 and hum=55", m)
	}
	if m[windowCountField] != 1.0 {
		t.Errorf("row B window_count = %v, want 1", m[windowCountField])
	}
}
