// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rollups

import (
	"fmt"
	"math"
	"time"

	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/pkg/block"
	"github.com/tviviano/ts-store/pkg/store"
)

const (
	defaultEdgeTolerance = 0.10
	defaultRetention     = "1y"
	// bytesPerAggField is a generous per-output-field byte estimate for a
	// compact schema record ("N":value): a few digits of index key + a JSON
	// number. Real records are smaller; over-estimating just rounds capacity
	// up, which the design wants (never retain LESS than requested).
	bytesPerAggField = 16
	dataBlockSize    = 4096
	indexBlockSize   = 4096
)

// canonicalWindow normalizes a window duration string to the largest clean
// unit, so equivalent windows produce the same target store name:
//
//	60m / 3600s -> 1h ;  24h -> 1d ;  7d -> 1w
//
// Durations that aren't a clean multiple of a larger unit keep their parsed
// nanosecond value rendered in the smallest sensible unit.
func canonicalWindow(windowStr string) (string, time.Duration, error) {
	d, err := duration.ParseDuration(windowStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid window %q: %w", windowStr, err)
	}
	if d <= 0 {
		return "", 0, fmt.Errorf("window must be positive: %q", windowStr)
	}

	week := 7 * 24 * time.Hour
	day := 24 * time.Hour
	switch {
	case d%week == 0:
		return fmt.Sprintf("%dw", d/week), d, nil
	case d%day == 0:
		return fmt.Sprintf("%dd", d/day), d, nil
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour), d, nil
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute), d, nil
	default:
		return fmt.Sprintf("%ds", d/time.Second), d, nil
	}
}

// derivedTargetName returns "<source>-<canonical-window>".
func derivedTargetName(sourceName, windowStr string) (string, error) {
	cw, _, err := canonicalWindow(windowStr)
	if err != nil {
		return "", err
	}
	return sourceName + "-" + cw, nil
}

// sizing is the result of deriving a target store's capacity from retention.
type sizing struct {
	numPartitions  uint32
	totalSize      int64         // bytes; 0 means use NumBlocks (V1 fallback, unused here)
	actualRetention time.Duration // what the created store will actually hold (>= requested)
	recordsToRetain int64
}

// deriveSizing computes target store capacity from retention + window, choosing
// a partition count so worst-case over-retention (1/P) stays within
// edgeTolerance, then rounding capacity UP so the store holds at least the
// requested retention.
func deriveSizing(retentionStr, windowStr, aggFields, aggDefault string, edgeTolerance float64) (sizing, error) {
	if retentionStr == "" {
		retentionStr = defaultRetention
	}
	retention, err := duration.ParseDuration(retentionStr)
	if err != nil {
		return sizing{}, fmt.Errorf("invalid retention %q: %w", retentionStr, err)
	}
	_, window, err := canonicalWindow(windowStr)
	if err != nil {
		return sizing{}, err
	}
	if retention < window {
		return sizing{}, fmt.Errorf("retention %s must be >= window %s", retentionStr, windowStr)
	}
	if edgeTolerance <= 0 {
		edgeTolerance = defaultEdgeTolerance
	}

	// records = ceil(retention / window)
	recordsToRetain := int64((retention + window - 1) / window)

	// Estimate per-record size: object header + one entry per agg output field
	// (+ window_count). estimateOutputFields is a generous upper bound.
	nFields := estimateOutputFields(aggFields, aggDefault) + 1 // +1 for window_count
	bytesPerRecord := int64(block.ObjectHeaderSize) + int64(nFields*bytesPerAggField)

	usableBytes := recordsToRetain * bytesPerRecord

	// Records that fit in one data block.
	usablePerBlock := int64(block.UsableDataSize(dataBlockSize)) - int64(block.ObjectHeaderSize)
	if usablePerBlock < 1 {
		usablePerBlock = 1
	}
	recordsPerBlock := usablePerBlock / bytesPerRecord
	if recordsPerBlock < 1 {
		recordsPerBlock = 1
	}

	// Pick partition count: smallest P with 1/P <= edgeTolerance, clamped [2,16].
	p := int64(math.Ceil(1.0 / edgeTolerance))
	if p < 2 {
		p = 2
	}
	if p > 16 {
		p = 16
	}

	// Blocks needed overall (round up), then per partition (round up). Ensure
	// every partition holds >= 1 block; if not, reduce P until it fits.
	totalBlocks := ceilDivInt64(recordsToRetain, recordsPerBlock)
	if totalBlocks < 1 {
		totalBlocks = 1
	}
	for p > 2 && totalBlocks < p {
		p--
	}
	blocksPerPartition := ceilDivInt64(totalBlocks, p)
	if blocksPerPartition < 1 {
		blocksPerPartition = 1
	}

	// TotalSize is split across partitions by store.Config:
	//   blocksPerPartition = TotalSize / P / DataBlockSize
	// so TotalSize = blocksPerPartition * P * DataBlockSize.
	totalSize := blocksPerPartition * p * int64(dataBlockSize)

	actualRecords := blocksPerPartition * recordsPerBlock * p
	actualRetention := time.Duration(actualRecords) * window

	_ = usableBytes // documented intent; capacity is block-rounded above
	return sizing{
		numPartitions:   uint32(p),
		totalSize:       totalSize,
		actualRetention: actualRetention,
		recordsToRetain: recordsToRetain,
	}, nil
}

// targetConfig builds a store.Config for the auto-created rollup target.
func targetConfig(name, basePath string, sz sizing) store.Config {
	return store.Config{
		Name:           name,
		Path:           basePath,
		DataBlockSize:  dataBlockSize,
		IndexBlockSize: indexBlockSize,
		DataType:       store.DataTypeSchema,
		StorageType:    store.StorageTypeV2Partitioned,
		NumPartitions:  sz.numPartitions,
		TotalSize:      sz.totalSize,
	}
}

func ceilDivInt64(a, b int64) int64 {
	if b == 0 {
		return a
	}
	return (a + b - 1) / b
}
