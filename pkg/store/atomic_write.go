// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"errors"
	"os"
)

// writeFileAtomic writes data to path via a temp file + fsync + rename so a
// crash mid-write can never leave a truncated file behind. Sidecar JSON files
// (schema.json, connection and alert configs, rollup configs) are load-bearing
// at open time — a torn schema.json makes the store refuse to open — so they
// must never be written in place. The rename is atomic because the temp file
// lives in the same directory as the target.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		err = errors.Join(err, f.Close())
		os.Remove(tmp)
		return err
	}
	// Sync before rename: otherwise the rename can be durable while the
	// contents are not, leaving an empty file after power loss.
	if err := f.Sync(); err != nil {
		err = errors.Join(err, f.Close())
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
