// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package unixsock

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/service"
)

// startTestListener brings up a listener on a short-path socket with one
// JSON store and returns the socket path and a valid API key.
func startTestListener(t *testing.T) (sockPath, storeName, key string) {
	t.Helper()

	// t.TempDir paths can exceed the ~104-char unix socket path limit.
	sockDir, err := os.MkdirTemp("", "uskt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath = filepath.Join(sockDir, "s.sock")

	dataDir := t.TempDir()
	cfg := &config.Config{
		Store: config.StoreConfig{
			BasePath:       dataDir,
			DataBlockSize:  4096,
			IndexBlockSize: 4096,
			NumBlocks:      64,
		},
	}
	km := apikey.NewManager(dataDir)
	svc := service.NewStoreService(cfg, km)
	t.Cleanup(func() { svc.CloseAll() })

	storeName = "sock-test"
	if _, err := svc.Create(&service.CreateStoreRequest{Name: storeName, DataType: "json"}); err != nil {
		t.Fatalf("create store: %v", err)
	}
	key, _, err = km.CreateForStore(storeName, "test")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	l := NewListener(sockPath, svc, km)
	if err := l.Start(); err != nil {
		t.Fatalf("listener start: %v", err)
	}
	t.Cleanup(func() { l.Stop() })
	return sockPath, storeName, key
}

// Regression test for issue #28: the 1s read deadline exists for
// interruptibility, but a timeout mid-line used to discard the bytes
// bufio had already consumed — a client pausing >1s inside a record lost
// the front of it and the tail then failed JSON parsing.
func TestPartialLineSurvivesReadDeadline(t *testing.T) {
	sockPath, storeName, key := startTestListener(t)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	fmt.Fprintf(conn, "AUTH %s %s\n", storeName, key)
	resp, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(resp) != "OK" {
		t.Fatalf("auth: %q err=%v", resp, err)
	}

	// Write the record in two chunks with a pause longer than the 1s
	// read deadline between them.
	if _, err := conn.Write([]byte(`{"temperature": 4`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := conn.Write([]byte("2}\n")); err != nil {
		t.Fatal(err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err = r.ReadString('\n')
	if err != nil {
		t.Fatalf("no response to split record: %v", err)
	}
	if !strings.HasPrefix(resp, "OK ") {
		t.Fatalf("split record rejected: %q (front of line was dropped)", strings.TrimSpace(resp))
	}

	// The connection must still work for subsequent complete lines.
	if _, err := conn.Write([]byte(`{"temperature": 43}` + "\n")); err != nil {
		t.Fatal(err)
	}
	resp, err = r.ReadString('\n')
	if err != nil || !strings.HasPrefix(resp, "OK ") {
		t.Fatalf("followup record failed: %q err=%v", strings.TrimSpace(resp), err)
	}
}

// TestClientTimestampEnvelope (issue #64): an optional
// {"timestamp": <ns>, "data": {...}} wrapper on data lines lets edge
// processes backfill buffered records with original timestamps; bare
// records keep the historical server-timestamp behavior.
func TestClientTimestampEnvelope(t *testing.T) {
	sockPath, storeName, key := startTestListener(t)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	send := func(line string) string {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return strings.TrimSpace(resp)
	}

	if resp := send(fmt.Sprintf("AUTH %s %s", storeName, key)); resp != "OK" {
		t.Fatalf("auth: %q", resp)
	}

	// Envelope: the ack must echo the CLIENT timestamp.
	if resp := send(`{"timestamp": 1000000, "data": {"temp": 22.5}}`); resp != "OK 1000000" {
		t.Errorf("envelope write ack = %q, want OK 1000000", resp)
	}

	// Bare record: server timestamp (later than the backfilled one).
	if resp := send(`{"temp": 22.6}`); !strings.HasPrefix(resp, "OK ") || resp == "OK 1000000" {
		t.Errorf("bare write ack = %q, want OK <server-ns>", resp)
	}

	// A record that merely CONTAINS a timestamp field among others is
	// stored as-is with a server timestamp — not treated as an envelope.
	if resp := send(`{"timestamp": 5, "temp": 22.7, "data": "x"}`); !strings.HasPrefix(resp, "OK ") || resp == "OK 5" {
		t.Errorf("3-key record ack = %q, want OK <server-ns>", resp)
	}

	// Malformed envelopes are rejected, not silently stored.
	if resp := send(`{"timestamp": -5, "data": {"temp": 1}}`); !strings.HasPrefix(resp, "ERROR") {
		t.Errorf("negative-timestamp envelope ack = %q, want ERROR", resp)
	}
	if resp := send(`{"timestamp": 2000000, "data": null}`); !strings.HasPrefix(resp, "ERROR") {
		t.Errorf("null-data envelope ack = %q, want ERROR", resp)
	}
	if resp := send(`{"timestamp": "notanumber", "data": {"temp": 1}}`); !strings.HasPrefix(resp, "ERROR") {
		t.Errorf("string-timestamp envelope ack = %q, want ERROR", resp)
	}

	// Backfill must still respect monotonicity: an envelope older than
	// the newest record gets the store's out-of-order error with detail.
	if resp := send(`{"timestamp": 999, "data": {"temp": 1}}`); !strings.HasPrefix(resp, "ERROR") {
		t.Errorf("stale envelope ack = %q, want ERROR", resp)
	}

	if resp := send("QUIT"); resp != "OK bye" {
		t.Errorf("quit ack = %q", resp)
	}
}
