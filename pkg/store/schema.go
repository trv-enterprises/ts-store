// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tviviano/ts-store/pkg/schema"
)

var (
	ErrSchemaNotSupported = errors.New("schema operations only supported for schema data type stores")
)

// GetSchemaSet returns the current schema set for schema stores.
// Returns nil for non-schema stores.
func (s *Store) GetSchemaSet() *schema.SchemaSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemaSet
}

// GetSchema returns the current schema version for schema stores.
func (s *Store) GetSchema() (*schema.Schema, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.meta.DataType != DataTypeSchema {
		return nil, ErrSchemaNotSupported
	}

	if s.schemaSet == nil {
		return nil, ErrSchemaRequired
	}

	return s.schemaSet.GetCurrentSchema()
}

// SetSchema adds a new schema version. For the first call, creates a new schema.
// For subsequent calls, validates that the new schema is append-only compatible.
func (s *Store) SetSchema(sch *schema.Schema) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.meta.DataType != DataTypeSchema {
		return 0, ErrSchemaNotSupported
	}

	if s.schemaSet == nil {
		s.schemaSet = schema.NewSchemaSet()
	}

	version, err := s.schemaSet.AddSchema(sch)
	if err != nil {
		return 0, err
	}

	// Persist schema to file
	if err := s.saveSchemaLocked(); err != nil {
		return 0, err
	}

	return version, nil
}

// ValidateAndCompact validates data against the schema and returns compact form.
// Only valid for schema stores.
func (s *Store) ValidateAndCompact(data []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.meta.DataType != DataTypeSchema {
		return nil, ErrSchemaNotSupported
	}

	if s.schemaSet == nil {
		return nil, ErrSchemaRequired
	}

	// Validate the data
	if err := s.schemaSet.ValidateData(data); err != nil {
		return nil, err
	}

	// Convert to compact format
	return s.schemaSet.FullToCompact(data)
}

// ExpandData converts compact data to full format.
// Only valid for schema stores.
func (s *Store) ExpandData(data []byte, schemaVersion int) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.meta.DataType != DataTypeSchema {
		return nil, ErrSchemaNotSupported
	}

	if s.schemaSet == nil {
		return nil, ErrSchemaRequired
	}

	return s.schemaSet.CompactToFull(data, schemaVersion)
}

// saveSchemaLocked persists the schema to disk. Lock must be held.
func (s *Store) saveSchemaLocked() error {
	schemaPath := filepath.Join(s.path, schemaFileName)

	data, err := json.MarshalIndent(s.schemaSet, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(schemaPath, data, 0644)
}

// loadSchema loads the schema from disk.
func (s *Store) loadSchema() error {
	if s.meta.DataType != DataTypeSchema {
		return nil // Not a schema store, nothing to load
	}

	schemaPath := filepath.Join(s.path, schemaFileName)

	data, err := os.ReadFile(schemaPath)
	if os.IsNotExist(err) {
		// No schema yet - that's ok for new schema stores
		s.schemaSet = nil
		return nil
	}
	if err != nil {
		return err
	}

	s.schemaSet = schema.NewSchemaSet()
	return json.Unmarshal(data, s.schemaSet)
}
