// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package schema

import (
	"encoding/json"
	"testing"
)

func TestSchemaValidation(t *testing.T) {
	tests := []struct {
		name    string
		schema  *Schema
		wantErr bool
	}{
		{
			name: "valid schema",
			schema: &Schema{
				Fields: []Field{
					{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
					{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
				},
			},
			wantErr: false,
		},
		{
			name: "empty fields",
			schema: &Schema{
				Fields: []Field{},
			},
			wantErr: true,
		},
		{
			name: "invalid index",
			schema: &Schema{
				Fields: []Field{
					{Index: 0, Name: "temperature", Type: FieldTypeFloat32},
				},
			},
			wantErr: true,
		},
		{
			name: "duplicate index",
			schema: &Schema{
				Fields: []Field{
					{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
					{Index: 1, Name: "humidity", Type: FieldTypeFloat32},
				},
			},
			wantErr: true,
		},
		{
			name: "duplicate name",
			schema: &Schema{
				Fields: []Field{
					{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
					{Index: 2, Name: "temperature", Type: FieldTypeFloat32},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid type",
			schema: &Schema{
				Fields: []Field{
					{Index: 1, Name: "temperature", Type: "invalid"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.schema.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFullToCompact(t *testing.T) {
	ss := NewSchemaSet()
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
			{Index: 3, Name: "sensor_id", Type: FieldTypeString},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema failed: %v", err)
	}

	input := []byte(`{"temperature": 72.5, "humidity": 45, "sensor_id": "living-room"}`)
	compact, err := ss.FullToCompact(input)
	if err != nil {
		t.Fatalf("FullToCompact failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(compact, &result); err != nil {
		t.Fatalf("Failed to unmarshal compact: %v", err)
	}

	if result["1"] != 72.5 {
		t.Errorf("Expected temperature=72.5, got %v", result["1"])
	}
	if result["2"] != float64(45) {
		t.Errorf("Expected humidity=45, got %v", result["2"])
	}
	if result["3"] != "living-room" {
		t.Errorf("Expected sensor_id=living-room, got %v", result["3"])
	}
}

func TestCompactToFull(t *testing.T) {
	ss := NewSchemaSet()
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
			{Index: 3, Name: "sensor_id", Type: FieldTypeString},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema failed: %v", err)
	}

	input := []byte(`{"1": 72.5, "2": 45, "3": "living-room"}`)
	full, err := ss.CompactToFull(input, 0)
	if err != nil {
		t.Fatalf("CompactToFull failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(full, &result); err != nil {
		t.Fatalf("Failed to unmarshal full: %v", err)
	}

	if result["temperature"] != 72.5 {
		t.Errorf("Expected temperature=72.5, got %v", result["temperature"])
	}
	if result["humidity"] != float64(45) {
		t.Errorf("Expected humidity=45, got %v", result["humidity"])
	}
	if result["sensor_id"] != "living-room" {
		t.Errorf("Expected sensor_id=living-room, got %v", result["sensor_id"])
	}
}

func TestRoundTrip(t *testing.T) {
	ss := NewSchemaSet()
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema failed: %v", err)
	}

	original := []byte(`{"temperature":72.5,"humidity":45}`)

	compact, err := ss.FullToCompact(original)
	if err != nil {
		t.Fatalf("FullToCompact failed: %v", err)
	}

	full, err := ss.CompactToFull(compact, 0)
	if err != nil {
		t.Fatalf("CompactToFull failed: %v", err)
	}

	var orig, result map[string]interface{}
	json.Unmarshal(original, &orig)
	json.Unmarshal(full, &result)

	if orig["temperature"] != result["temperature"] {
		t.Errorf("temperature mismatch: %v != %v", orig["temperature"], result["temperature"])
	}
	if orig["humidity"] != result["humidity"] {
		t.Errorf("humidity mismatch: %v != %v", orig["humidity"], result["humidity"])
	}
}

func TestSchemaEvolution(t *testing.T) {
	ss := NewSchemaSet()

	// Add version 1
	v1, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema v1 failed: %v", err)
	}
	if v1 != 1 {
		t.Errorf("Expected version 1, got %d", v1)
	}

	// Add version 2 (append field)
	v2, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
			{Index: 3, Name: "pressure", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema v2 failed: %v", err)
	}
	if v2 != 2 {
		t.Errorf("Expected version 2, got %d", v2)
	}

	// Read old data (v1) with new schema should work
	oldData := []byte(`{"1": 72.5, "2": 45}`)
	full, err := ss.CompactToFull(oldData, 1)
	if err != nil {
		t.Fatalf("CompactToFull v1 data failed: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal(full, &result)
	if result["temperature"] != 72.5 {
		t.Errorf("Expected temperature=72.5, got %v", result["temperature"])
	}
}

func TestSchemaEvolutionRejectModify(t *testing.T) {
	ss := NewSchemaSet()

	// Add version 1
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema v1 failed: %v", err)
	}

	// Try to modify existing field - should fail
	_, err = ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temp", Type: FieldTypeFloat32}, // Changed name
		},
	})
	if err == nil {
		t.Error("Expected error when modifying existing field")
	}
}

func TestValidateData(t *testing.T) {
	ss := NewSchemaSet()
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema failed: %v", err)
	}

	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{
			name:    "valid full",
			data:    `{"temperature": 72.5, "humidity": 45}`,
			wantErr: false,
		},
		{
			name:    "valid compact",
			data:    `{"1": 72.5, "2": 45}`,
			wantErr: false,
		},
		{
			name:    "unknown field in full",
			data:    `{"temperature": 72.5, "unknown": 45}`,
			wantErr: true,
		},
		{
			name:    "unknown index in compact",
			data:    `{"1": 72.5, "99": 45}`,
			wantErr: true,
		},
		{
			name:    "partial data ok",
			data:    `{"temperature": 72.5}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ss.ValidateData([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateData() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSchemaSetSerialization(t *testing.T) {
	ss := NewSchemaSet()
	_, err := ss.AddSchema(&Schema{
		Fields: []Field{
			{Index: 1, Name: "temperature", Type: FieldTypeFloat32},
			{Index: 2, Name: "humidity", Type: FieldTypeFloat32},
		},
	})
	if err != nil {
		t.Fatalf("AddSchema failed: %v", err)
	}

	// Serialize
	data, err := json.Marshal(ss)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Deserialize
	ss2 := NewSchemaSet()
	if err := json.Unmarshal(data, ss2); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify it works
	input := []byte(`{"temperature": 72.5, "humidity": 45}`)
	compact, err := ss2.FullToCompact(input)
	if err != nil {
		t.Fatalf("FullToCompact after deserialize failed: %v", err)
	}

	if string(compact) == "" {
		t.Error("Expected non-empty compact output")
	}
}
