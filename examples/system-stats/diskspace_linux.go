// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"syscall"
)

func readDiskSpace() DiskSpace {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return DiskSpace{}
	}

	total := int64(stat.Blocks) * int64(stat.Bsize)
	available := int64(stat.Bavail) * int64(stat.Bsize)
	used := total - available
	pct := 0
	if total > 0 {
		pct = int(used * 100 / total)
	}

	return DiskSpace{
		Total:     total,
		Used:      used,
		Available: available,
		Pct:       pct,
	}
}
