// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package schema provides compact JSON encoding/decoding using schemas.
// Schemas map field names to numeric indices, allowing JSON to be stored
// in a compact format: {"1": value, "2": value} instead of {"field_name": value}.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

var (
	ErrInvalidSchema     = errors.New("invalid schema")
	ErrFieldNotInSchema  = errors.New("field not found in schema")
	ErrInvalidFieldType  = errors.New("invalid field type")
	ErrMissingField      = errors.New("missing required field")
	ErrInvalidJSON       = errors.New("invalid JSON")
	ErrVersionMismatch   = errors.New("schema version mismatch")
	ErrInvalidCompactKey = errors.New("invalid compact JSON key (must be numeric)")
)

// FieldType represents the type of a schema field.
type FieldType string

const (
	FieldTypeInt8    FieldType = "int8"
	FieldTypeInt16   FieldType = "int16"
	FieldTypeInt32   FieldType = "int32"
	FieldTypeInt64   FieldType = "int64"
	FieldTypeUint8   FieldType = "uint8"
	FieldTypeUint16  FieldType = "uint16"
	FieldTypeUint32  FieldType = "uint32"
	FieldTypeUint64  FieldType = "uint64"
	FieldTypeFloat32 FieldType = "float32"
	FieldTypeFloat64 FieldType = "float64"
	FieldTypeBool    FieldType = "bool"
	FieldTypeString  FieldType = "string"
)

// ValidFieldTypes is the set of valid field types.
var ValidFieldTypes = map[FieldType]bool{
	FieldTypeInt8:    true,
	FieldTypeInt16:   true,
	FieldTypeInt32:   true,
	FieldTypeInt64:   true,
	FieldTypeUint8:   true,
	FieldTypeUint16:  true,
	FieldTypeUint32:  true,
	FieldTypeUint64:  true,
	FieldTypeFloat32: true,
	FieldTypeFloat64: true,
	FieldTypeBool:    true,
	FieldTypeString:  true,
}

// Field represents a single field in a schema.
type Field struct {
	Index int       `json:"index"` // Numeric index (1-based)
	Name  string    `json:"name"`  // Field name
	Type  FieldType `json:"type"`  // Field type
}

// Schema represents a versioned schema for compact JSON encoding.
type Schema struct {
	Version int     `json:"version"` // Schema version (1-based)
	Fields  []Field `json:"fields"`  // Fields in this schema version
}

// SchemaSet holds multiple schema versions for a store.
// Allows reading old data with newer schemas.
type SchemaSet struct {
	CurrentVersion int                `json:"current_version"`
	Schemas        map[int]*Schema    `json:"schemas"` // version -> schema
	nameToIndex    map[int]map[string]int // version -> name -> index (cached)
	indexToName    map[int]map[int]string // version -> index -> name (cached)
}

// NewSchemaSet creates a new empty SchemaSet.
func NewSchemaSet() *SchemaSet {
	return &SchemaSet{
		CurrentVersion: 0,
		Schemas:        make(map[int]*Schema),
		nameToIndex:    make(map[int]map[string]int),
		indexToName:    make(map[int]map[int]string),
	}
}

// AddSchema adds a new schema version. Returns the new version number.
// For version > 1, new fields must only be appended (indices must be greater than existing).
func (ss *SchemaSet) AddSchema(s *Schema) (int, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}

	// Determine version
	newVersion := ss.CurrentVersion + 1
	s.Version = newVersion

	// If not first version, validate fields are append-only
	if ss.CurrentVersion > 0 {
		current := ss.Schemas[ss.CurrentVersion]
		if err := validateAppendOnly(current, s); err != nil {
			return 0, err
		}
	}

	ss.Schemas[newVersion] = s
	ss.CurrentVersion = newVersion
	ss.buildCache(newVersion, s)

	return newVersion, nil
}

// GetSchema returns a schema by version.
func (ss *SchemaSet) GetSchema(version int) (*Schema, error) {
	s, ok := ss.Schemas[version]
	if !ok {
		return nil, fmt.Errorf("%w: version %d not found", ErrVersionMismatch, version)
	}
	return s, nil
}

// GetCurrentSchema returns the current (latest) schema.
func (ss *SchemaSet) GetCurrentSchema() (*Schema, error) {
	if ss.CurrentVersion == 0 {
		return nil, ErrInvalidSchema
	}
	return ss.Schemas[ss.CurrentVersion], nil
}

// Validate checks that a schema is valid.
func (s *Schema) Validate() error {
	if len(s.Fields) == 0 {
		return fmt.Errorf("%w: no fields defined", ErrInvalidSchema)
	}

	seen := make(map[int]bool)
	names := make(map[string]bool)

	for _, f := range s.Fields {
		if f.Index <= 0 {
			return fmt.Errorf("%w: field index must be positive: %d", ErrInvalidSchema, f.Index)
		}
		if f.Name == "" {
			return fmt.Errorf("%w: field name is required", ErrInvalidSchema)
		}
		if !ValidFieldTypes[f.Type] {
			return fmt.Errorf("%w: %s", ErrInvalidFieldType, f.Type)
		}
		if seen[f.Index] {
			return fmt.Errorf("%w: duplicate field index: %d", ErrInvalidSchema, f.Index)
		}
		if names[f.Name] {
			return fmt.Errorf("%w: duplicate field name: %s", ErrInvalidSchema, f.Name)
		}
		seen[f.Index] = true
		names[f.Name] = true
	}

	return nil
}

// validateAppendOnly ensures new schema only appends fields to existing schema.
func validateAppendOnly(current, new *Schema) error {
	// All existing fields must remain unchanged
	existingIndices := make(map[int]Field)
	for _, f := range current.Fields {
		existingIndices[f.Index] = f
	}

	for _, f := range new.Fields {
		if existing, ok := existingIndices[f.Index]; ok {
			// Field exists - must match exactly
			if existing.Name != f.Name || existing.Type != f.Type {
				return fmt.Errorf("%w: cannot modify existing field %d", ErrInvalidSchema, f.Index)
			}
		}
	}

	return nil
}

// buildCache builds the name/index lookup caches for a schema version.
func (ss *SchemaSet) buildCache(version int, s *Schema) {
	ss.nameToIndex[version] = make(map[string]int)
	ss.indexToName[version] = make(map[int]string)

	for _, f := range s.Fields {
		ss.nameToIndex[version][f.Name] = f.Index
		ss.indexToName[version][f.Index] = f.Name
	}
}

// FullToCompact converts full JSON to compact JSON using the current schema.
// Input: {"temperature": 72.5, "humidity": 45}
// Output: {"1": 72.5, "2": 45}
func (ss *SchemaSet) FullToCompact(data []byte) ([]byte, error) {
	if ss.CurrentVersion == 0 {
		return nil, ErrInvalidSchema
	}

	var full map[string]interface{}
	if err := json.Unmarshal(data, &full); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	nameToIdx := ss.nameToIndex[ss.CurrentVersion]
	compact := make(map[string]interface{})

	for name, value := range full {
		idx, ok := nameToIdx[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrFieldNotInSchema, name)
		}
		compact[strconv.Itoa(idx)] = value
	}

	return json.Marshal(compact)
}

// CompactToFull converts compact JSON to full JSON.
// If version is 0, uses the current schema version.
// Input: {"1": 72.5, "2": 45}
// Output: {"temperature": 72.5, "humidity": 45}
func (ss *SchemaSet) CompactToFull(data []byte, version int) ([]byte, error) {
	if version == 0 {
		version = ss.CurrentVersion
	}

	if version == 0 {
		return nil, ErrInvalidSchema
	}

	idxToName, ok := ss.indexToName[version]
	if !ok {
		return nil, fmt.Errorf("%w: version %d", ErrVersionMismatch, version)
	}

	var compact map[string]interface{}
	if err := json.Unmarshal(data, &compact); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	full := make(map[string]interface{})

	for key, value := range compact {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidCompactKey, key)
		}
		name, ok := idxToName[idx]
		if !ok {
			// Field not in this schema version - skip (allows forward compatibility)
			continue
		}
		full[name] = value
	}

	return json.Marshal(full)
}

// ValidateData validates that JSON data conforms to the current schema.
// Accepts either full or compact JSON format.
func (ss *SchemaSet) ValidateData(data []byte) error {
	if ss.CurrentVersion == 0 {
		return ErrInvalidSchema
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	// Determine if compact or full format by checking first key
	isCompact := false
	for key := range parsed {
		_, err := strconv.Atoi(key)
		isCompact = (err == nil)
		break
	}

	if isCompact {
		return ss.validateCompact(parsed)
	}
	return ss.validateFull(parsed)
}

func (ss *SchemaSet) validateFull(data map[string]interface{}) error {
	nameToIdx := ss.nameToIndex[ss.CurrentVersion]

	for name := range data {
		if _, ok := nameToIdx[name]; !ok {
			return fmt.Errorf("%w: %s", ErrFieldNotInSchema, name)
		}
	}

	return nil
}

func (ss *SchemaSet) validateCompact(data map[string]interface{}) error {
	idxToName := ss.indexToName[ss.CurrentVersion]

	for key := range data {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidCompactKey, key)
		}
		if _, ok := idxToName[idx]; !ok {
			return fmt.Errorf("%w: index %d", ErrFieldNotInSchema, idx)
		}
	}

	return nil
}

// MarshalJSON implements json.Marshaler for SchemaSet.
func (ss *SchemaSet) MarshalJSON() ([]byte, error) {
	type alias SchemaSet
	return json.Marshal(&struct {
		*alias
	}{
		alias: (*alias)(ss),
	})
}

// UnmarshalJSON implements json.Unmarshaler for SchemaSet.
func (ss *SchemaSet) UnmarshalJSON(data []byte) error {
	type alias SchemaSet
	aux := &struct {
		*alias
	}{
		alias: (*alias)(ss),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	// Rebuild caches
	ss.nameToIndex = make(map[int]map[string]int)
	ss.indexToName = make(map[int]map[int]string)

	for version, schema := range ss.Schemas {
		ss.buildCache(version, schema)
	}

	return nil
}
