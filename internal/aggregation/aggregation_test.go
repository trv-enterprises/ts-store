// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package aggregation

import (
	"math"
	"testing"
	"time"
)

func makeConfig(t *testing.T, window time.Duration, fields []FieldAgg, defaultFunc AggFunc, numericMap map[string]bool) *Config {
	t.Helper()
	cfg, err := NewConfig(window, fields, defaultFunc, numericMap)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	return cfg
}

// --- Config / ParseFieldAggs tests ---

func TestNewConfig_InvalidWindow(t *testing.T) {
	_, err := NewConfig(0, nil, AggAvg, nil)
	if err == nil {
		t.Fatal("expected error for zero window")
	}
}

func TestNewConfig_InvalidDefaultFunc(t *testing.T) {
	_, err := NewConfig(time.Minute, nil, "bogus", nil)
	if err == nil {
		t.Fatal("expected error for invalid default func")
	}
}

func TestNewConfig_InvalidFieldFunc(t *testing.T) {
	_, err := NewConfig(time.Minute, []FieldAgg{{Field: "x", Function: "bogus"}}, "", nil)
	if err == nil {
		t.Fatal("expected error for invalid field func")
	}
}

func TestParseFieldAggs(t *testing.T) {
	tests := []struct {
		input string
		want  int
		err   bool
	}{
		{"", 0, false},
		{"cpu:avg", 1, false},
		{"cpu:avg,mem:max", 2, false},
		{"cpu:avg, mem:max , disk:sum", 3, false},
		{"bad", 0, true},
		{"cpu:bogus", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseFieldAggs(tt.input)
		if (err != nil) != tt.err {
			t.Errorf("ParseFieldAggs(%q): err=%v, wantErr=%v", tt.input, err, tt.err)
			continue
		}
		if err == nil && len(got) != tt.want {
			t.Errorf("ParseFieldAggs(%q): got %d fields, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestFuncForField(t *testing.T) {
	cfg := makeConfig(t, time.Minute, []FieldAgg{{Field: "cpu", Function: AggMax}}, AggAvg, map[string]bool{"cpu": true, "mem": true, "name": false})

	if fn := cfg.FuncForField("cpu", true); fn != AggMax {
		t.Errorf("cpu: got %s, want max", fn)
	}
	if fn := cfg.FuncForField("mem", true); fn != AggAvg {
		t.Errorf("mem: got %s, want avg (default)", fn)
	}
	if fn := cfg.FuncForField("name", false); fn != AggLast {
		t.Errorf("name: got %s, want last", fn)
	}
}

// --- Batch aggregation tests ---

func TestAggregateBatch_Avg(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"val": 30.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	if results[0].Count != 3 {
		t.Errorf("count: got %d, want 3", results[0].Count)
	}
	val := results[0].Data["val"].(float64)
	if math.Abs(val-20.0) > 0.001 {
		t.Errorf("avg: got %f, want 20.0", val)
	}
}

func TestAggregateBatch_Sum(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggSum, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"val": 30.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	// Last window from Flush is partial → sum should be nil
	if results[0].Data["val"] != nil {
		t.Errorf("partial sum should be nil, got %v", results[0].Data["val"])
	}
}

func TestAggregateBatch_SumFullWindow(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggSum, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		// Window 0
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		// Window 1 (triggers close of window 0)
		{Timestamp: 1*ns + 1, Data: map[string]interface{}{"val": 5.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}
	// First window is a full close (not partial)
	val := results[0].Data["val"].(float64)
	if math.Abs(val-30.0) > 0.001 {
		t.Errorf("sum window 0: got %f, want 30.0", val)
	}
	// Second window is partial (flushed)
	if results[1].Data["val"] != nil {
		t.Errorf("partial sum should be nil, got %v", results[1].Data["val"])
	}
}

func TestAggregateBatch_MaxMin(t *testing.T) {
	cfg := makeConfig(t, time.Minute,
		[]FieldAgg{
			{Field: "high", Function: AggMax},
			{Field: "low", Function: AggMin},
		}, "", map[string]bool{"high": true, "low": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"high": 5.0, "low": 3.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"high": 15.0, "low": 1.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"high": 8.0, "low": 7.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	if v := results[0].Data["high"].(float64); v != 15.0 {
		t.Errorf("max: got %f, want 15.0", v)
	}
	if v := results[0].Data["low"].(float64); v != 1.0 {
		t.Errorf("min: got %f, want 1.0", v)
	}
}

func TestAggregateBatch_Count(t *testing.T) {
	cfg := makeConfig(t, time.Minute,
		[]FieldAgg{{Field: "val", Function: AggCount}},
		"", map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 1.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 2.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"val": 3.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	if v := results[0].Data["val"].(int); v != 3 {
		t.Errorf("count: got %d, want 3", v)
	}
}

func TestAggregateBatch_Last(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggLast, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"val": 30.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	if v := results[0].Data["val"].(float64); v != 30.0 {
		t.Errorf("last: got %f, want 30.0", v)
	}
}

func TestAggregateBatch_MultipleWindows(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		// Window 0
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		// Window 1
		{Timestamp: 1*ns + 1, Data: map[string]interface{}{"val": 100.0}},
		{Timestamp: 1*ns + 2, Data: map[string]interface{}{"val": 200.0}},
		// Window 2
		{Timestamp: 2*ns + 1, Data: map[string]interface{}{"val": 50.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(results))
	}

	expected := []float64{15.0, 150.0, 50.0}
	for i, exp := range expected {
		got := results[i].Data["val"].(float64)
		if math.Abs(got-exp) > 0.001 {
			t.Errorf("window %d: got %f, want %f", i, got, exp)
		}
	}
}

func TestAggregateBatch_NonNumericField(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true, "name": false})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0, "name": "first"}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0, "name": "second"}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 1 {
		t.Fatalf("expected 1 window, got %d", len(results))
	}
	// Non-numeric should use "last"
	if v := results[0].Data["name"].(string); v != "second" {
		t.Errorf("name: got %s, want second", v)
	}
	// Numeric should use default (avg)
	val := results[0].Data["val"].(float64)
	if math.Abs(val-15.0) > 0.001 {
		t.Errorf("val: got %f, want 15.0", val)
	}
}

func TestAggregateBatch_Empty(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, nil)
	results := AggregateBatch(nil, cfg)
	if results != nil {
		t.Errorf("expected nil for empty input, got %v", results)
	}
}

func TestAggregateBatch_WindowTimestamps(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true})

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 5, Data: map[string]interface{}{"val": 10.0}},
		// Second window triggers close of first
		{Timestamp: 1*ns + 5, Data: map[string]interface{}{"val": 20.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}
	// First window end = aligned window start + window
	if results[0].Timestamp != 1*ns {
		t.Errorf("window 0 timestamp: got %d, want %d", results[0].Timestamp, 1*ns)
	}
	if results[0].Partial != false {
		t.Errorf("window 0 should not be partial")
	}
	if results[1].Partial != true {
		t.Errorf("window 1 should be partial (flushed)")
	}
}

// --- Accumulator streaming tests ---

func TestAccumulator_Add(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true})
	acc := NewAccumulator(cfg)

	ns := int64(time.Minute)

	// First record - window opens, no result
	r := acc.Add(0*ns+1, map[string]interface{}{"val": 10.0})
	if r != nil {
		t.Fatal("expected nil on first record")
	}

	// Second record in same window
	r = acc.Add(0*ns+2, map[string]interface{}{"val": 20.0})
	if r != nil {
		t.Fatal("expected nil in same window")
	}

	// Record in next window - closes previous
	r = acc.Add(1*ns+1, map[string]interface{}{"val": 100.0})
	if r == nil {
		t.Fatal("expected result on window close")
	}
	if r.Count != 2 {
		t.Errorf("count: got %d, want 2", r.Count)
	}
	val := r.Data["val"].(float64)
	if math.Abs(val-15.0) > 0.001 {
		t.Errorf("avg: got %f, want 15.0", val)
	}
}

func TestAccumulator_Flush(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, map[string]bool{"val": true})
	acc := NewAccumulator(cfg)

	ns := int64(time.Minute)
	acc.Add(0*ns+1, map[string]interface{}{"val": 10.0})
	acc.Add(0*ns+2, map[string]interface{}{"val": 20.0})

	r := acc.Flush()
	if r == nil {
		t.Fatal("expected result from Flush")
	}
	if !r.Partial {
		t.Error("flushed result should be partial")
	}
	if r.Count != 2 {
		t.Errorf("count: got %d, want 2", r.Count)
	}
}

func TestAccumulator_FlushEmpty(t *testing.T) {
	cfg := makeConfig(t, time.Minute, nil, AggAvg, nil)
	acc := NewAccumulator(cfg)

	r := acc.Flush()
	if r != nil {
		t.Error("expected nil from Flush on empty accumulator")
	}
}

func TestAccumulator_JSONTypeSniffing(t *testing.T) {
	// nil NumericMap = JSON store, type-sniff from values
	cfg := makeConfig(t, time.Minute, nil, AggAvg, nil)
	acc := NewAccumulator(cfg)

	ns := int64(time.Minute)
	acc.Add(0*ns+1, map[string]interface{}{"cpu": 10.0, "host": "web1"})
	acc.Add(0*ns+2, map[string]interface{}{"cpu": 20.0, "host": "web2"})

	r := acc.Flush()
	if r == nil {
		t.Fatal("expected result")
	}
	// cpu is float64 → numeric → avg
	val := r.Data["cpu"].(float64)
	if math.Abs(val-15.0) > 0.001 {
		t.Errorf("cpu avg: got %f, want 15.0", val)
	}
	// host is string → non-numeric → last
	if r.Data["host"] != "web2" {
		t.Errorf("host: got %v, want web2", r.Data["host"])
	}
}

func TestBuildNumericMap_Nil(t *testing.T) {
	m := BuildNumericMap(nil)
	if m != nil {
		t.Error("expected nil for nil schema set")
	}
}

// --- Multi-function aggregation tests ---

func TestParseFieldAggs_MultiFunction(t *testing.T) {
	tests := []struct {
		input     string
		wantLen   int
		wantFuncs int // expected number of functions for first field
		err       bool
	}{
		{"cpu:avg+sum+min+max", 1, 4, false},
		{"cpu:avg+sum,mem:max", 2, 2, false},
		{"cpu:avg+bogus", 0, 0, true},
		{"temp:avg + sum + min", 1, 3, false}, // with spaces
	}
	for _, tt := range tests {
		got, err := ParseFieldAggs(tt.input)
		if (err != nil) != tt.err {
			t.Errorf("ParseFieldAggs(%q): err=%v, wantErr=%v", tt.input, err, tt.err)
			continue
		}
		if err == nil {
			if len(got) != tt.wantLen {
				t.Errorf("ParseFieldAggs(%q): got %d fields, want %d", tt.input, len(got), tt.wantLen)
			}
			if len(got) > 0 && len(got[0].Functions) != tt.wantFuncs {
				t.Errorf("ParseFieldAggs(%q): got %d funcs, want %d", tt.input, len(got[0].Functions), tt.wantFuncs)
			}
		}
	}
}

func TestNewConfig_MultiDefault(t *testing.T) {
	cfg, err := NewConfig(time.Minute, nil, "avg,sum,min,max", nil)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if len(cfg.Defaults) != 4 {
		t.Errorf("expected 4 defaults, got %d", len(cfg.Defaults))
	}
	expected := []AggFunc{AggAvg, AggSum, AggMin, AggMax}
	for i, fn := range expected {
		if cfg.Defaults[i] != fn {
			t.Errorf("default[%d]: got %s, want %s", i, cfg.Defaults[i], fn)
		}
	}
	// Backward compat: Default should be first
	if cfg.Default != AggAvg {
		t.Errorf("Default: got %s, want avg", cfg.Default)
	}
}

func TestFuncsForField(t *testing.T) {
	fields := []FieldAgg{
		{Field: "cpu", Functions: []AggFunc{AggMax, AggMin}},
	}
	cfg := makeConfig(t, time.Minute, fields, "avg,sum", map[string]bool{"cpu": true, "mem": true, "name": false})

	// Explicit field config
	funcs := cfg.FuncsForField("cpu", true)
	if len(funcs) != 2 || funcs[0] != AggMax || funcs[1] != AggMin {
		t.Errorf("cpu: got %v, want [max min]", funcs)
	}

	// Default for numeric
	funcs = cfg.FuncsForField("mem", true)
	if len(funcs) != 2 || funcs[0] != AggAvg || funcs[1] != AggSum {
		t.Errorf("mem: got %v, want [avg sum]", funcs)
	}

	// Non-numeric always uses last
	funcs = cfg.FuncsForField("name", false)
	if len(funcs) != 1 || funcs[0] != AggLast {
		t.Errorf("name: got %v, want [last]", funcs)
	}
}

func TestAggregateBatch_MultiDefault(t *testing.T) {
	cfg, err := NewConfig(time.Minute, nil, "avg,sum,min,max", map[string]bool{"val": true})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"val": 10.0}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"val": 20.0}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"val": 30.0}},
		// Next window triggers emit of first
		{Timestamp: 1*ns + 1, Data: map[string]interface{}{"val": 100.0}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}

	// First window (full, not partial)
	data := results[0].Data
	if v, ok := data["val_avg"].(float64); !ok || math.Abs(v-20.0) > 0.001 {
		t.Errorf("val_avg: got %v, want 20.0", data["val_avg"])
	}
	if v, ok := data["val_sum"].(float64); !ok || math.Abs(v-60.0) > 0.001 {
		t.Errorf("val_sum: got %v, want 60.0", data["val_sum"])
	}
	if v, ok := data["val_min"].(float64); !ok || math.Abs(v-10.0) > 0.001 {
		t.Errorf("val_min: got %v, want 10.0", data["val_min"])
	}
	if v, ok := data["val_max"].(float64); !ok || math.Abs(v-30.0) > 0.001 {
		t.Errorf("val_max: got %v, want 30.0", data["val_max"])
	}
	// Original field name should not exist
	if _, ok := data["val"]; ok {
		t.Error("val should not exist when multi-function is used")
	}
}

func TestAggregateBatch_PerFieldMultiFunction(t *testing.T) {
	fields, err := ParseFieldAggs("temp:avg+min+max,count:sum")
	if err != nil {
		t.Fatalf("ParseFieldAggs: %v", err)
	}
	cfg, err := NewConfig(time.Minute, fields, "last", map[string]bool{"temp": true, "count": true, "name": false})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}

	ns := int64(time.Minute)
	records := []TimestampedRecord{
		{Timestamp: 0*ns + 1, Data: map[string]interface{}{"temp": 68.0, "count": 5.0, "name": "a"}},
		{Timestamp: 0*ns + 2, Data: map[string]interface{}{"temp": 72.0, "count": 3.0, "name": "b"}},
		{Timestamp: 0*ns + 3, Data: map[string]interface{}{"temp": 70.0, "count": 2.0, "name": "c"}},
		// Trigger emit
		{Timestamp: 1*ns + 1, Data: map[string]interface{}{"temp": 80.0, "count": 1.0, "name": "d"}},
	}

	results := AggregateBatch(records, cfg)
	if len(results) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(results))
	}

	data := results[0].Data
	// temp: multi-function
	if v := data["temp_avg"].(float64); math.Abs(v-70.0) > 0.001 {
		t.Errorf("temp_avg: got %f, want 70.0", v)
	}
	if v := data["temp_min"].(float64); math.Abs(v-68.0) > 0.001 {
		t.Errorf("temp_min: got %f, want 68.0", v)
	}
	if v := data["temp_max"].(float64); math.Abs(v-72.0) > 0.001 {
		t.Errorf("temp_max: got %f, want 72.0", v)
	}
	// count: single function
	if v := data["count"].(float64); math.Abs(v-10.0) > 0.001 {
		t.Errorf("count: got %f, want 10.0", v)
	}
	// name: single function (last)
	if v := data["name"].(string); v != "c" {
		t.Errorf("name: got %s, want c", v)
	}
}
