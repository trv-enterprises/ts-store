// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package unixsock provides Unix domain socket support for low-latency local data ingestion.
package unixsock

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/service"
)

// Listener manages Unix socket connections for data ingestion.
type Listener struct {
	socketPath   string
	storeService *service.StoreService
	keyManager   *apikey.Manager
	listener     net.Listener
	wg           sync.WaitGroup
	done         chan struct{}
	mu           sync.Mutex
}

// NewListener creates a new Unix socket listener.
func NewListener(socketPath string, storeService *service.StoreService, keyManager *apikey.Manager) *Listener {
	return &Listener{
		socketPath:   socketPath,
		storeService: storeService,
		keyManager:   keyManager,
		done:         make(chan struct{}),
	}
}

// Start begins listening on the Unix socket.
func (l *Listener) Start() error {
	// Ensure socket directory exists
	socketDir := filepath.Dir(l.socketPath)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Remove existing socket file if present
	if err := os.Remove(l.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create Unix socket listener
	listener, err := net.Listen("unix", l.socketPath)
	if err != nil {
		return fmt.Errorf("failed to create Unix socket: %w", err)
	}

	// Set permissions (readable/writable by owner and group)
	if err := os.Chmod(l.socketPath, 0660); err != nil {
		listener.Close()
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	l.mu.Lock()
	l.listener = listener
	l.mu.Unlock()

	log.Printf("Unix socket listening on %s", l.socketPath)

	// Accept connections in goroutine
	go l.acceptLoop()

	return nil
}

// Stop gracefully shuts down the listener.
func (l *Listener) Stop() error {
	close(l.done)

	l.mu.Lock()
	if l.listener != nil {
		l.listener.Close()
	}
	l.mu.Unlock()

	// Wait for all connections to finish
	l.wg.Wait()

	// Clean up socket file
	os.Remove(l.socketPath)

	return nil
}

func (l *Listener) acceptLoop() {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
				log.Printf("Unix socket accept error: %v", err)
				continue
			}
		}

		l.wg.Add(1)
		go l.handleConnection(conn)
	}
}

// Connection protocol:
// 1. Client sends: AUTH <store-name> <api-key>\n
// 2. Server responds: OK\n or ERROR <message>\n
// 3. Client sends data lines: {"field": "value"}\n
// 4. Server responds per line: OK <timestamp>\n or ERROR <message>\n
//
// For schema stores, send full JSON and it will be auto-compacted.
// Timestamps are auto-generated (current time in nanoseconds).

func (l *Listener) handleConnection(conn net.Conn) {
	defer l.wg.Done()
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Read AUTH line
	authLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	authLine = strings.TrimSpace(authLine)

	parts := strings.SplitN(authLine, " ", 3)
	if len(parts) != 3 || strings.ToUpper(parts[0]) != "AUTH" {
		writer.WriteString("ERROR invalid auth format, expected: AUTH <store> <api-key>\n")
		writer.Flush()
		return
	}

	storeName := parts[1]
	apiKey := parts[2]

	// Validate API key
	if _, err := l.keyManager.Validate(storeName, apiKey); err != nil {
		writer.WriteString("ERROR authentication failed\n")
		writer.Flush()
		return
	}

	// Get or open the store
	st, err := l.storeService.GetOrOpen(storeName)
	if err != nil {
		writer.WriteString(fmt.Sprintf("ERROR failed to open store: %s\n", err.Error()))
		writer.Flush()
		return
	}

	writer.WriteString("OK\n")
	writer.Flush()

	// Process data lines
	for {
		select {
		case <-l.done:
			return
		default:
		}

		// Set read deadline for interruptibility
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))

		line, err := reader.ReadString('\n')
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Handle QUIT command
		if strings.ToUpper(line) == "QUIT" {
			writer.WriteString("OK bye\n")
			writer.Flush()
			return
		}

		// Parse and store the data
		timestamp := time.Now().UnixNano()

		// Validate JSON
		var js json.RawMessage
		if err := json.Unmarshal([]byte(line), &js); err != nil {
			writer.WriteString(fmt.Sprintf("ERROR invalid JSON: %s\n", err.Error()))
			writer.Flush()
			continue
		}

		// Store the object
		handle, err := st.PutObject(timestamp, []byte(line))
		if err != nil {
			writer.WriteString(fmt.Sprintf("ERROR store failed: %s\n", err.Error()))
			writer.Flush()
			continue
		}

		writer.WriteString(fmt.Sprintf("OK %d\n", handle.Timestamp))
		writer.Flush()
	}
}

// SocketPath returns the path to the Unix socket.
func (l *Listener) SocketPath() string {
	return l.socketPath
}
