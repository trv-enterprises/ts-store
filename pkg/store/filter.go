// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package store

import "bytes"

// MatchesFilter returns true if data contains the filter pattern.
// If filter is empty, always returns true.
func MatchesFilter(data []byte, filter string, ignoreCase bool) bool {
	if filter == "" {
		return true
	}
	if ignoreCase {
		return bytes.Contains(bytes.ToLower(data), bytes.ToLower([]byte(filter)))
	}
	return bytes.Contains(data, []byte(filter))
}
