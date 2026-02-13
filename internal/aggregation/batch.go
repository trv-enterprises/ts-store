// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package aggregation

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
