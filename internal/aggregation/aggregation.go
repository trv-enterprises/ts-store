// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package aggregation provides time-windowed aggregation for numeric time-series data.
// It supports streaming (Add/Flush) and batch modes, with configurable per-field
// aggregation functions (sum, avg, max, min, count, last).
package aggregation

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tviviano/ts-store/pkg/schema"
)

// AggFunc represents an aggregation function.
type AggFunc string

const (
	AggSum   AggFunc = "sum"
	AggAvg   AggFunc = "avg"
	AggMax   AggFunc = "max"
	AggMin   AggFunc = "min"
	AggCount AggFunc = "count"
	AggLast  AggFunc = "last"
)

// ValidAggFuncs is the set of valid aggregation functions.
var ValidAggFuncs = map[AggFunc]bool{
	AggSum: true, AggAvg: true, AggMax: true, AggMin: true,
	AggCount: true, AggLast: true,
}

// FieldAgg maps a field name to an aggregation function.
type FieldAgg struct {
	Field    string
	Function AggFunc
}

// Config holds aggregation configuration.
type Config struct {
	Window     time.Duration
	Fields     []FieldAgg
	Default    AggFunc
	NumericMap map[string]bool // field name -> is numeric (pre-computed)
	fieldFuncs map[string]AggFunc
}

// AggResult represents the output of one aggregation window.
type AggResult struct {
	Timestamp int64                  // window end timestamp (nanoseconds)
	Count     int                    // number of records in this window
	Partial   bool                   // true if flushed before window closed
	Data      map[string]interface{} // aggregated field values
}

// TimestampedRecord is a single record with timestamp for batch aggregation.
type TimestampedRecord struct {
	Timestamp int64
	Data      map[string]interface{}
}

// NewConfig creates and validates an aggregation config.
func NewConfig(window time.Duration, fields []FieldAgg, defaultFunc AggFunc, numericMap map[string]bool) (*Config, error) {
	if window <= 0 {
		return nil, fmt.Errorf("aggregation window must be positive")
	}

	if defaultFunc != "" && !ValidAggFuncs[defaultFunc] {
		return nil, fmt.Errorf("invalid default aggregation function: %s", string(defaultFunc))
	}

	fieldFuncs := make(map[string]AggFunc)
	for _, fa := range fields {
		if !ValidAggFuncs[fa.Function] {
			return nil, fmt.Errorf("invalid aggregation function for field %s: %s", fa.Field, string(fa.Function))
		}
		fieldFuncs[fa.Field] = fa.Function
	}

	return &Config{
		Window:     window,
		Fields:     fields,
		Default:    defaultFunc,
		NumericMap: numericMap,
		fieldFuncs: fieldFuncs,
	}, nil
}

// FuncForField returns the aggregation function for a given field.
// Priority: explicit field config > default > "last" for non-numeric.
func (c *Config) FuncForField(field string, isNumeric bool) AggFunc {
	if fn, ok := c.fieldFuncs[field]; ok {
		return fn
	}
	if isNumeric && c.Default != "" {
		return c.Default
	}
	if isNumeric {
		return AggLast
	}
	return AggLast
}

// IsNumeric checks whether a field is numeric, using the pre-computed map
// or falling back to type-sniffing the value.
func (c *Config) IsNumeric(field string, value interface{}) bool {
	if c.NumericMap != nil {
		if v, ok := c.NumericMap[field]; ok {
			return v
		}
	}
	// Type-sniff for JSON stores (json.Unmarshal produces float64 for numbers)
	switch value.(type) {
	case float64, int, int64, float32:
		return true
	}
	return false
}

// ParseFieldAggs parses "field1:func1,field2:func2" into []FieldAgg.
func ParseFieldAggs(s string) ([]FieldAgg, error) {
	if s == "" {
		return nil, nil
	}

	var result []FieldAgg
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pieces := strings.SplitN(part, ":", 2)
		if len(pieces) != 2 {
			return nil, fmt.Errorf("invalid field aggregation: %q (expected field:function)", part)
		}
		field := strings.TrimSpace(pieces[0])
		fn := AggFunc(strings.TrimSpace(pieces[1]))
		if !ValidAggFuncs[fn] {
			return nil, fmt.Errorf("invalid aggregation function %q for field %q", fn, field)
		}
		result = append(result, FieldAgg{Field: field, Function: fn})
	}
	return result, nil
}

// BuildNumericMap builds a numeric field map from a schema.SchemaSet.
// Returns nil if schemaSet is nil (JSON store — type-sniff at runtime).
func BuildNumericMap(ss *schema.SchemaSet) map[string]bool {
	if ss == nil || ss.CurrentVersion == 0 {
		return nil
	}
	s, err := ss.GetCurrentSchema()
	if err != nil {
		return nil
	}
	m := make(map[string]bool)
	for _, f := range s.Fields {
		m[f.Name] = isNumericFieldType(f.Type)
	}
	return m
}

// isNumericFieldType returns true for numeric schema field types.
func isNumericFieldType(ft schema.FieldType) bool {
	switch ft {
	case schema.FieldTypeInt8, schema.FieldTypeInt16, schema.FieldTypeInt32, schema.FieldTypeInt64,
		schema.FieldTypeUint8, schema.FieldTypeUint16, schema.FieldTypeUint32, schema.FieldTypeUint64,
		schema.FieldTypeFloat32, schema.FieldTypeFloat64:
		return true
	}
	return false
}

// toFloat64 converts a value to float64, or returns ok=false.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int16:
		return float64(n), true
	case int8:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint8:
		return float64(n), true
	}
	return 0, false
}

// fieldState tracks the running state of a single field's aggregation.
type fieldState struct {
	fn    AggFunc
	sum   float64
	max   float64
	min   float64
	count int
	last  interface{}
}

func newFieldState(fn AggFunc) *fieldState {
	return &fieldState{
		fn:  fn,
		max: -math.MaxFloat64,
		min: math.MaxFloat64,
	}
}

func (fs *fieldState) add(value interface{}) {
	fs.last = value
	fs.count++

	f, ok := toFloat64(value)
	if !ok {
		return
	}

	fs.sum += f
	if f > fs.max {
		fs.max = f
	}
	if f < fs.min {
		fs.min = f
	}
}

func (fs *fieldState) result(partial bool) interface{} {
	if fs.count == 0 {
		return nil
	}
	switch fs.fn {
	case AggSum:
		if partial {
			return nil // partial sum is misleading
		}
		return fs.sum
	case AggAvg:
		return fs.sum / float64(fs.count)
	case AggMax:
		return fs.max
	case AggMin:
		return fs.min
	case AggCount:
		return fs.count
	case AggLast:
		return fs.last
	}
	return fs.last
}
