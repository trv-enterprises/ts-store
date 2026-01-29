// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1

//go:build !linux

package main

// readDiskSpace is a stub for non-Linux platforms.
// Disk space collection only works on Linux.
func readDiskSpace() DiskSpace {
	return DiskSpace{}
}
