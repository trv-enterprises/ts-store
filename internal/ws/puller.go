// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package ws

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/pkg/store"
)

// Puller handles outbound pull connections (remote -> ts-store).
type Puller struct {
	mu        sync.RWMutex
	store     *store.Store
	storeName string
	config    store.WSConnection

	conn          *websocket.Conn
	status        string
	lastTimestamp int64
	messagesRecv  int64
	errors        int64
	lastError     string

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPuller creates a new outbound pull connection.
func NewPuller(st *store.Store, storeName string, config store.WSConnection) *Puller {
	return &Puller{
		store:     st,
		storeName: storeName,
		config:    config,
		status:    "disconnected",
		stopCh:    make(chan struct{}),
	}
}

// ID returns the connection ID.
func (p *Puller) ID() string {
	return p.config.ID
}

// Status returns the current connection status.
func (p *Puller) Status() ConnectionStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return ConnectionStatus{
		ID:            p.config.ID,
		Mode:          p.config.Mode,
		URL:           p.config.URL,
		Format:        p.config.Format,
		Status:        p.status,
		CreatedAt:     p.config.CreatedAt,
		LastTimestamp: p.lastTimestamp,
		MessagesRecv:  p.messagesRecv,
		Errors:        p.errors,
		LastError:     p.lastError,
	}
}

// Start begins the pull connection with auto-reconnect.
func (p *Puller) Start() error {
	p.wg.Add(1)
	go p.runLoop()
	return nil
}

// Stop stops the pull connection.
func (p *Puller) Stop() error {
	close(p.stopCh)
	p.wg.Wait()

	p.mu.Lock()
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	p.status = "disconnected"
	p.mu.Unlock()

	return nil
}

// runLoop is the main connection loop with auto-reconnect.
func (p *Puller) runLoop() {
	defer p.wg.Done()

	retryDelay := time.Second
	maxRetryDelay := 60 * time.Second

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		err := p.connect()
		if err != nil {
			p.setError(err.Error())
			retryDelay = min(retryDelay*2, maxRetryDelay)

			select {
			case <-p.stopCh:
				return
			case <-time.After(retryDelay):
				continue
			}
		}

		// Reset retry delay on successful connection
		retryDelay = time.Second

		// Run the pull loop
		err = p.pullLoop()
		if err != nil {
			p.setError(err.Error())
		}

		// Clean up connection
		p.mu.Lock()
		if p.conn != nil {
			p.conn.Close()
			p.conn = nil
		}
		p.status = "disconnected"
		p.mu.Unlock()

		// Wait before reconnecting
		select {
		case <-p.stopCh:
			return
		case <-time.After(retryDelay):
		}
	}
}

// connect establishes a WebSocket connection to the remote server.
func (p *Puller) connect() error {
	p.mu.Lock()
	p.status = "connecting"
	p.mu.Unlock()

	// Build HTTP header from config
	header := http.Header{}
	for k, v := range p.config.Headers {
		header.Set(k, v)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(p.config.URL, header)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.conn = conn
	p.status = "connected"
	p.lastError = ""
	p.mu.Unlock()

	return nil
}

// pullLoop receives data from the remote server and stores it.
func (p *Puller) pullLoop() error {
	for {
		select {
		case <-p.stopCh:
			return nil
		default:
		}

		// Set read deadline to allow periodic stop checks
		p.mu.RLock()
		conn := p.conn
		p.mu.RUnlock()

		if conn == nil {
			return nil
		}

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			// Check if it's a timeout (expected for periodic checks)
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				continue
			}
			return err
		}

		if err := p.processMessage(message); err != nil {
			p.setError(err.Error())
			// Continue processing even on individual message errors
		}
	}
}

// PullMessage represents a message received from the remote server.
type PullMessage struct {
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// processMessage processes an incoming message and stores it.
func (p *Puller) processMessage(message []byte) error {
	var msg PullMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return err
	}

	// Use provided timestamp or generate one
	timestamp := msg.Timestamp
	if timestamp == 0 {
		timestamp = time.Now().UnixNano()
	}

	// Convert data based on format
	var dataBytes []byte
	if p.config.Format == "compact" && p.store.DataType() == store.DataTypeSchema {
		// Validate and compact the data
		compacted, err := p.store.ValidateAndCompact(msg.Data)
		if err != nil {
			return err
		}
		dataBytes = compacted
	} else {
		dataBytes = msg.Data
	}

	// Store the data
	handle, err := p.store.PutObject(timestamp, dataBytes)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.lastTimestamp = handle.Timestamp
	atomic.AddInt64(&p.messagesRecv, 1)
	p.mu.Unlock()

	return nil
}

// setError sets the last error and increments error count.
func (p *Puller) setError(msg string) {
	p.mu.Lock()
	p.lastError = msg
	p.status = "error"
	atomic.AddInt64(&p.errors, 1)
	p.mu.Unlock()
}
