// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package aggregation

import "time"

// Accumulator is a streaming aggregation engine for WebSocket/MQTT push paths.
// It collects records and emits an AggResult when a time window closes.
type Accumulator struct {
	config      *Config
	windowNanos int64 // window duration in nanoseconds

	windowStart int64                    // start of current window (nanoseconds)
	windowEnd   int64                    // end of current window (nanoseconds)
	fields      map[string]*fieldState   // field name -> running state
	fieldFuncs  map[string][]AggFunc     // field name -> functions to apply
	knownFields map[string]bool          // tracks which fields we've seen (for type sniffing)
	count       int
}

// NewAccumulator creates a new streaming accumulator.
func NewAccumulator(config *Config) *Accumulator {
	return &Accumulator{
		config:      config,
		windowNanos: config.Window.Nanoseconds(),
		fields:      make(map[string]*fieldState),
		fieldFuncs:  make(map[string][]AggFunc),
		knownFields: make(map[string]bool),
	}
}

// Add feeds a record into the accumulator. Returns an AggResult if the record's
// timestamp falls outside the current window (closing the previous window),
// or nil if the window is still open.
func (a *Accumulator) Add(timestamp int64, data map[string]interface{}) *AggResult {
	var result *AggResult

	// If this is the first record, initialize the window
	if a.count == 0 {
		a.initWindow(timestamp)
	}

	// Check if this record falls outside the current window
	if timestamp >= a.windowEnd {
		// Close current window and emit result
		result = a.emit(false)
		// Start new window aligned to the record's timestamp
		a.initWindow(timestamp)
	}

	// Accumulate the record
	a.accumulate(data)
	return result
}

// Flush forces emission of the current window as a partial result.
// Returns nil if no records have been accumulated.
func (a *Accumulator) Flush() *AggResult {
	if a.count == 0 {
		return nil
	}
	return a.emit(true)
}

// WindowDuration returns the configured window duration.
func (a *Accumulator) WindowDuration() time.Duration {
	return a.config.Window
}

// initWindow sets up a new window starting at the given timestamp,
// aligned to window boundaries.
func (a *Accumulator) initWindow(timestamp int64) {
	// Align window start to window boundaries
	a.windowStart = (timestamp / a.windowNanos) * a.windowNanos
	a.windowEnd = a.windowStart + a.windowNanos
	a.fields = make(map[string]*fieldState)
	a.fieldFuncs = make(map[string][]AggFunc)
	a.count = 0
}

// accumulate adds a single record's fields to the running state.
func (a *Accumulator) accumulate(data map[string]interface{}) {
	a.count++
	for field, value := range data {
		fs, ok := a.fields[field]
		if !ok {
			isNumeric := a.config.IsNumeric(field, value)
			// Cache the type decision for JSON stores
			if a.config.NumericMap == nil {
				if !a.knownFields[field] {
					a.knownFields[field] = true
				}
			}
			funcs := a.config.FuncsForField(field, isNumeric)
			a.fieldFuncs[field] = funcs
			// Use first function for fieldState.fn (for backward compat with result())
			fn := funcs[0]
			fs = newFieldState(fn)
			a.fields[field] = fs
		}
		fs.add(value)
	}
}

// emit closes the current window and returns the aggregated result.
func (a *Accumulator) emit(partial bool) *AggResult {
	if a.count == 0 {
		return nil
	}

	data := make(map[string]interface{})
	for field, fs := range a.fields {
		funcs := a.fieldFuncs[field]
		if len(funcs) == 1 {
			// Single function: use original field name
			data[field] = fs.resultFor(funcs[0], partial)
		} else {
			// Multiple functions: emit suffixed field names
			for _, fn := range funcs {
				key := field + "_" + string(fn)
				data[key] = fs.resultFor(fn, partial)
			}
		}
	}

	result := &AggResult{
		Timestamp: a.windowEnd,
		Count:     a.count,
		Partial:   partial,
		Data:      data,
	}

	// Reset for next window
	a.fields = make(map[string]*fieldState)
	a.fieldFuncs = make(map[string][]AggFunc)
	a.count = 0
	return result
}
