// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// Package version holds the tsstore release version so both the CLI and API
// handlers can report it.
package version

// Version is the build's version string.
//
// Release builds INJECT it via -ldflags (see Makefile's LDFLAGS and the
// Dockerfile's VERSION build arg), which is authoritative. The default here
// is deliberately "dev": a plain `go build` with no ldflags produces a
// binary that says so, rather than one claiming to be whatever release the
// constant was last bumped to (issue #182).
//
// Nothing rewrites this line any more: `make version-bump` is retired
// (issue #182), so there is exactly one place the version comes from.
var Version = "dev"
