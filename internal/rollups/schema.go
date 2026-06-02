// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rollups

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/pkg/schema"
)

// windowCountField is the per-record sample-count field written on every rollup
// record. Load-bearing: consumers need it to compute correct count-weighted
// averages when re-aggregating to coarser tiers. Never drop it.
const windowCountField = "window_count"

// outputField describes one field of the derived target (rollup) schema.
type outputField struct {
	name      string
	fieldType schema.FieldType
}

// deriveOutputFields computes the rollup target's fields from the agg spec and
// the SOURCE schema's numeric map, mirroring the accumulator's emit() naming:
//   - a field aggregated by ONE function keeps its source name (e.g. "cpu")
//   - a field aggregated by MULTIPLE functions emits "<field>_<func>" each
//     (e.g. "cpu_avg", "cpu_max")
//
// Defaults apply to every numeric source field not given an explicit spec.
// Types: numeric aggregates -> float64; count -> uint32; last/non-numeric ->
// string. The window_count field (uint32) is always appended.
func deriveOutputFields(aggFields, aggDefault string, srcNumeric map[string]bool) ([]outputField, error) {
	explicit, err := aggregation.ParseFieldAggs(aggFields)
	if err != nil {
		return nil, err
	}

	defaults, err := parseFuncs(aggDefault)
	if err != nil {
		return nil, err
	}

	// field -> functions. Explicit specs win; defaults fill in numeric source
	// fields not explicitly listed.
	specs := make(map[string][]aggregation.AggFunc)
	explicitSet := make(map[string]bool)
	for _, fa := range explicit {
		funcs := fa.Functions
		if len(funcs) == 0 && fa.Function != "" {
			funcs = []aggregation.AggFunc{fa.Function}
		}
		specs[fa.Field] = funcs
		explicitSet[fa.Field] = true
	}
	if len(defaults) > 0 {
		for field, isNum := range srcNumeric {
			if isNum && !explicitSet[field] {
				specs[field] = defaults
			}
		}
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("no aggregation fields resolved: set agg_fields and/or agg_default")
	}

	// Deterministic field order (sorted) so the derived schema is stable across
	// runs and re-creates.
	fields := make([]string, 0, len(specs))
	for f := range specs {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	var out []outputField
	for _, field := range fields {
		funcs := specs[field]
		if len(funcs) == 1 {
			out = append(out, outputField{
				name:      field,
				fieldType: aggResultType(funcs[0], srcNumeric[field]),
			})
			continue
		}
		for _, fn := range funcs {
			out = append(out, outputField{
				name:      field + "_" + string(fn),
				fieldType: aggResultType(fn, srcNumeric[field]),
			})
		}
	}

	out = append(out, outputField{name: windowCountField, fieldType: schema.FieldTypeUint32})
	return out, nil
}

// deriveTargetSchema builds a schema.Schema (field indices 1..N) from the agg
// spec and source numeric map.
func deriveTargetSchema(aggFields, aggDefault string, srcNumeric map[string]bool) (*schema.Schema, error) {
	fields, err := deriveOutputFields(aggFields, aggDefault, srcNumeric)
	if err != nil {
		return nil, err
	}
	sch := &schema.Schema{Fields: make([]schema.Field, 0, len(fields))}
	for i, f := range fields {
		sch.Fields = append(sch.Fields, schema.Field{
			Index: i + 1,
			Name:  f.name,
			Type:  f.fieldType,
		})
	}
	return sch, nil
}

// estimateOutputFields returns a count of derived output fields for sizing.
// Without the source schema it can't know which fields the default applies to,
// so it counts explicit specs (expanding multi-func) and adds a small cushion
// when a default is set. Used only for capacity estimation (rounds up anyway).
func estimateOutputFields(aggFields, aggDefault string) int {
	explicit, err := aggregation.ParseFieldAggs(aggFields)
	if err != nil {
		return 8 // conservative fallback
	}
	n := 0
	for _, fa := range explicit {
		if len(fa.Functions) > 0 {
			n += len(fa.Functions)
		} else {
			n++
		}
	}
	defaults, _ := parseFuncs(aggDefault)
	if len(defaults) > 0 {
		// Cushion for default-covered source fields (unknown count here).
		n += 8 * len(defaults)
	}
	if n < 1 {
		n = 8
	}
	return n
}

func aggResultType(fn aggregation.AggFunc, srcNumeric bool) schema.FieldType {
	switch fn {
	case aggregation.AggCount:
		return schema.FieldTypeUint32
	case aggregation.AggSum, aggregation.AggAvg, aggregation.AggMax, aggregation.AggMin:
		return schema.FieldTypeFloat64
	case aggregation.AggLast:
		if srcNumeric {
			return schema.FieldTypeFloat64
		}
		return schema.FieldTypeString
	default:
		return schema.FieldTypeFloat64
	}
}

func parseFuncs(s string) ([]aggregation.AggFunc, error) {
	if s == "" {
		return nil, nil
	}
	var funcs []aggregation.AggFunc
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fn := aggregation.AggFunc(part)
		if !aggregation.ValidAggFuncs[fn] {
			return nil, fmt.Errorf("invalid aggregation function: %s", part)
		}
		funcs = append(funcs, fn)
	}
	return funcs, nil
}
