// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidJSON = errors.New("invalid JSON data")
)

// PutJSON stores a JSON object at the given timestamp.
// The value is marshaled to JSON and stored across blocks as needed.
func (s *Store) PutJSON(timestamp int64, value any) (*ObjectHandle, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidJSON
	}
	return s.PutObject(timestamp, data)
}

// PutJSONNow stores a JSON object with the current timestamp.
func (s *Store) PutJSONNow(value any) (*ObjectHandle, error) {
	return s.PutJSON(time.Now().UnixNano(), value)
}

// GetJSON retrieves a JSON object by handle and unmarshals it into the provided value.
func (s *Store) GetJSON(handle *ObjectHandle, value any) error {
	data, err := s.GetObject(handle)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// GetJSONByTime retrieves a JSON object by timestamp and unmarshals it.
func (s *Store) GetJSONByTime(timestamp int64, value any) (*ObjectHandle, error) {
	data, handle, err := s.GetObjectByTime(timestamp)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, ErrInvalidJSON
	}
	return handle, nil
}

// GetJSONByBlock retrieves a JSON object by block number and unmarshals it.
func (s *Store) GetJSONByBlock(blockNum uint32, value any) (*ObjectHandle, error) {
	data, handle, err := s.GetObjectByBlock(blockNum)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return nil, ErrInvalidJSON
	}
	return handle, nil
}

// GetJSONRaw retrieves a JSON object as raw bytes (for when you don't know the structure).
func (s *Store) GetJSONRaw(handle *ObjectHandle) (json.RawMessage, error) {
	data, err := s.GetObject(handle)
	if err != nil {
		return nil, err
	}
	// Validate it's valid JSON
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, ErrInvalidJSON
	}
	return raw, nil
}

// GetJSONRawByTime retrieves a JSON object as raw bytes by timestamp.
func (s *Store) GetJSONRawByTime(timestamp int64) (json.RawMessage, *ObjectHandle, error) {
	data, handle, err := s.GetObjectByTime(timestamp)
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, ErrInvalidJSON
	}
	return raw, handle, nil
}

// GetJSONRawByBlock retrieves a JSON object as raw bytes by block number.
func (s *Store) GetJSONRawByBlock(blockNum uint32) (json.RawMessage, *ObjectHandle, error) {
	data, handle, err := s.GetObjectByBlock(blockNum)
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, ErrInvalidJSON
	}
	return raw, handle, nil
}

// GetOldestJSON retrieves the N oldest JSON objects as raw messages.
func (s *Store) GetOldestJSON(limit int) ([]json.RawMessage, []*ObjectHandle, error) {
	handles, err := s.GetOldestObjects(limit)
	if err != nil {
		return nil, nil, err
	}

	results := make([]json.RawMessage, 0, len(handles))
	validHandles := make([]*ObjectHandle, 0, len(handles))

	for _, h := range handles {
		data, err := s.GetObject(h)
		if err != nil {
			continue
		}
		var raw json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			// Skip non-JSON entries
			continue
		}
		results = append(results, raw)
		validHandles = append(validHandles, h)
	}

	return results, validHandles, nil
}

// GetNewestJSON retrieves the N newest JSON objects as raw messages.
func (s *Store) GetNewestJSON(limit int) ([]json.RawMessage, []*ObjectHandle, error) {
	handles, err := s.GetNewestObjects(limit)
	if err != nil {
		return nil, nil, err
	}

	results := make([]json.RawMessage, 0, len(handles))
	validHandles := make([]*ObjectHandle, 0, len(handles))

	for _, h := range handles {
		data, err := s.GetObject(h)
		if err != nil {
			continue
		}
		var raw json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			// Skip non-JSON entries
			continue
		}
		results = append(results, raw)
		validHandles = append(validHandles, h)
	}

	return results, validHandles, nil
}
