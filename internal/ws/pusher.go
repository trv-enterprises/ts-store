// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
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

// Pusher handles outbound push connections (ts-store -> remote).
type Pusher struct {
	mu        sync.RWMutex
	store     *store.Store
	storeName string
	config    store.WSConnection

	conn          *websocket.Conn
	status        string
	lastTimestamp int64
	messagesSent  int64
	errors        int64
	lastError     string

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPusher creates a new outbound push connection.
func NewPusher(st *store.Store, storeName string, config store.WSConnection) *Pusher {
	return &Pusher{
		store:         st,
		storeName:     storeName,
		config:        config,
		status:        "disconnected",
		lastTimestamp: config.From,
		stopCh:        make(chan struct{}),
	}
}

// ID returns the connection ID.
func (p *Pusher) ID() string {
	return p.config.ID
}

// Status returns the current connection status.
func (p *Pusher) Status() ConnectionStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return ConnectionStatus{
		ID:            p.config.ID,
		Mode:          p.config.Mode,
		URL:           p.config.URL,
		From:          p.config.From,
		Format:        p.config.Format,
		Status:        p.status,
		CreatedAt:     p.config.CreatedAt,
		LastTimestamp: p.lastTimestamp,
		MessagesSent:  p.messagesSent,
		Errors:        p.errors,
		LastError:     p.lastError,
	}
}

// Start begins the push connection with auto-reconnect.
func (p *Pusher) Start() error {
	p.wg.Add(1)
	go p.runLoop()
	return nil
}

// Stop stops the push connection.
func (p *Pusher) Stop() error {
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
func (p *Pusher) runLoop() {
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

		// Run the push loop
		err = p.pushLoop()
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
func (p *Pusher) connect() error {
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

// pushLoop sends data to the remote server.
func (p *Pusher) pushLoop() error {
	// Poll for new data and send it
	pollInterval := 100 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return nil
		case <-ticker.C:
			if err := p.sendNewData(); err != nil {
				return err
			}
		}
	}
}

// sendNewData sends any new data since lastTimestamp.
func (p *Pusher) sendNewData() error {
	p.mu.RLock()
	lastTs := p.lastTimestamp
	conn := p.conn
	p.mu.RUnlock()

	if conn == nil {
		return nil
	}

	// Get objects since last timestamp
	var handles []*store.ObjectHandle
	var err error

	if lastTs == 0 {
		// Get all objects from the beginning
		handles, err = p.store.GetOldestObjects(100)
	} else {
		// Get objects after last timestamp
		endTime := time.Now().UnixNano()
		handles, err = p.store.GetObjectsInRange(lastTs+1, endTime, 100)
	}

	if err != nil {
		return err
	}

	if len(handles) == 0 {
		return nil
	}

	for _, handle := range handles {
		data, err := p.store.GetObject(handle)
		if err != nil {
			continue
		}

		// Format the data based on config
		var payload any
		if p.config.Format == "compact" || p.store.DataType() != store.DataTypeSchema {
			payload = json.RawMessage(data)
		} else {
			// Expand schema data
			expanded, err := p.store.ExpandData(data, 0)
			if err == nil {
				payload = json.RawMessage(expanded)
			} else {
				payload = json.RawMessage(data)
			}
		}

		msg := struct {
			Type      string `json:"type"`
			Timestamp int64  `json:"timestamp"`
			Data      any    `json:"data"`
		}{
			Type:      "data",
			Timestamp: handle.Timestamp,
			Data:      payload,
		}

		p.mu.Lock()
		err = p.conn.WriteJSON(msg)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		p.lastTimestamp = handle.Timestamp
		atomic.AddInt64(&p.messagesSent, 1)
		p.mu.Unlock()
	}

	return nil
}

// setError sets the last error and increments error count.
func (p *Pusher) setError(msg string) {
	p.mu.Lock()
	p.lastError = msg
	p.status = "error"
	atomic.AddInt64(&p.errors, 1)
	p.mu.Unlock()
}
