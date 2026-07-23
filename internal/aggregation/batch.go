// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package aggregation

import "fmt"

// AggregateBatch performs batch aggregation on a sorted slice of records.
// Records must be sorted by timestamp ascending. Returns one AggResult per window.
func AggregateBatch(records []TimestampedRecord, config *Config) []AggResult {
	if len(records) == 0 {
		return nil
	}

	acc := NewAccumulator(config)
	var results []AggResult

	for _, rec := range records {
		result := acc.Add(rec.Timestamp, rec.Data)
		if result != nil {
			results = append(results, *result)
		}
	}

	// Flush the last window (partial if incomplete)
	if last := acc.Flush(); last != nil {
		results = append(results, *last)
	}

	return results
}

// GroupedAggResult is the aggregation output for one series: the distinct value
// of the group field, plus that series' windows. GroupValue is nil for the
// remainder series holding records that lacked the group field entirely.
type GroupedAggResult struct {
	GroupValue interface{}
	Results    []AggResult
}

// AggregateBatchGrouped partitions records by the value of groupField, then runs
// AggregateBatch independently on each partition — so multi-series data (e.g. one
// row per container per tick) downsamples per series instead of squashing every
// series into a shared time bucket.
//
// Records must be sorted by timestamp ascending; each partition is a subsequence
// and therefore stays ascending. Partitions are returned in first-seen order for
// deterministic output. Records missing groupField (or with a null value) collect
// into a single remainder partition with GroupValue == nil.
//
// The group field is a partition key, not an aggregated value: it is deliberately
// not fed to the aggregation funcs here. The caller injects the series identity
// into each output row from GroupValue.
func AggregateBatchGrouped(records []TimestampedRecord, groupField string, config *Config) []GroupedAggResult {
	if len(records) == 0 {
		return nil
	}

	// Partition, preserving first-seen order and per-partition ascending order.
	order := make([]string, 0)
	buckets := make(map[string][]TimestampedRecord)
	values := make(map[string]interface{}) // canonical key -> original group value

	for _, rec := range records {
		raw, ok := rec.Data[groupField]
		if !ok {
			raw = nil // missing field -> remainder series
		}
		key := fmt.Sprint(raw) // canonical map key; nil and JSON null both -> "<nil>"
		if _, seen := buckets[key]; !seen {
			order = append(order, key)
			values[key] = raw
		}
		buckets[key] = append(buckets[key], rec)
	}

	grouped := make([]GroupedAggResult, 0, len(order))
	for _, key := range order {
		grouped = append(grouped, GroupedAggResult{
			GroupValue: values[key],
			Results:    AggregateBatch(buckets[key], config),
		})
	}
	return grouped
}
