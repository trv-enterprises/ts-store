// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package version holds the tsstore release version so both the CLI and API
// handlers can report it.
package version

// Version is rewritten by `make version-bump` (see Makefile) during a
// release; the sed there matches this exact declaration.
var Version = "v0.20.4"
