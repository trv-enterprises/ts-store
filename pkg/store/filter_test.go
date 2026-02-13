// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import "testing"

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		filter     string
		ignoreCase bool
		want       bool
	}{
		{
			name:       "empty filter matches everything",
			data:       []byte(`{"sensor": "temp-01"}`),
			filter:     "",
			ignoreCase: false,
			want:       true,
		},
		{
			name:       "exact match case sensitive",
			data:       []byte(`{"sensor": "temp-01"}`),
			filter:     "temp-01",
			ignoreCase: false,
			want:       true,
		},
		{
			name:       "no match case sensitive",
			data:       []byte(`{"sensor": "temp-01"}`),
			filter:     "TEMP-01",
			ignoreCase: false,
			want:       false,
		},
		{
			name:       "match case insensitive",
			data:       []byte(`{"sensor": "temp-01"}`),
			filter:     "TEMP-01",
			ignoreCase: true,
			want:       true,
		},
		{
			name:       "partial match",
			data:       []byte(`{"building": "BUILDING A", "floor": 3}`),
			filter:     "BUILDING",
			ignoreCase: false,
			want:       true,
		},
		{
			name:       "partial match case insensitive",
			data:       []byte(`{"building": "BUILDING A", "floor": 3}`),
			filter:     "building a",
			ignoreCase: true,
			want:       true,
		},
		{
			name:       "no match",
			data:       []byte(`{"sensor": "temp-01"}`),
			filter:     "humidity",
			ignoreCase: false,
			want:       false,
		},
		{
			name:       "empty data no match",
			data:       []byte{},
			filter:     "test",
			ignoreCase: false,
			want:       false,
		},
		{
			name:       "empty data empty filter matches",
			data:       []byte{},
			filter:     "",
			ignoreCase: false,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesFilter(tt.data, tt.filter, tt.ignoreCase)
			if got != tt.want {
				t.Errorf("MatchesFilter(%q, %q, %v) = %v, want %v",
					string(tt.data), tt.filter, tt.ignoreCase, got, tt.want)
			}
		})
	}
}
