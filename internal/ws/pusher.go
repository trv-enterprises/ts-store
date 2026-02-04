// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/internal/duration"
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

	accumulator *aggregation.Accumulator // nil if no aggregation
	aggConfig   *aggregation.Config      // nil if no aggregation

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPusher creates a new outbound push connection.
func NewPusher(st *store.Store, storeName string, config store.WSConnection) *Pusher {
	p := &Pusher{
		store:         st,
		storeName:     storeName,
		config:        config,
		status:        "disconnected",
		lastTimestamp: config.From,
		stopCh:        make(chan struct{}),
	}

	// Initialize aggregation if configured
	if config.AggWindow != "" {
		if err := p.initAggregation(); err != nil {
			log.Printf("WS push %s: aggregation init failed: %v (continuing without aggregation)", config.ID, err)
		}
	}

	return p
}

// initAggregation sets up the accumulator from config.
func (p *Pusher) initAggregation() error {
	window, err := duration.ParseDuration(p.config.AggWindow)
	if err != nil {
		return err
	}

	fields, err := aggregation.ParseFieldAggs(p.config.AggFields)
	if err != nil {
		return err
	}

	numericMap := aggregation.BuildNumericMap(p.store.GetSchemaSet())

	cfg, err := aggregation.NewConfig(window, fields, aggregation.AggFunc(p.config.AggDefault), numericMap)
	if err != nil {
		return err
	}

	p.aggConfig = cfg
	p.accumulator = aggregation.NewAccumulator(cfg)
	return nil
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
		ID:               p.config.ID,
		Mode:             p.config.Mode,
		URL:              p.config.URL,
		From:             p.config.From,
		Format:           p.config.Format,
		Filter:           p.config.Filter,
		FilterIgnoreCase: p.config.FilterIgnoreCase,
		AggWindow:        p.config.AggWindow,
		AggFields:        p.config.AggFields,
		AggDefault:       p.config.AggDefault,
		Status:           p.status,
		CreatedAt:        p.config.CreatedAt,
		LastTimestamp:     p.lastTimestamp,
		MessagesSent:     p.messagesSent,
		Errors:           p.errors,
		LastError:        p.lastError,
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
	// Flush any remaining aggregated data
	if p.accumulator != nil && p.conn != nil {
		if result := p.accumulator.Flush(); result != nil {
			p.sendAggResult(result)
		}
	}
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

	// Set up aggregation flush ticker if configured
	var flushTicker *time.Ticker
	var flushCh <-chan time.Time
	if p.accumulator != nil {
		flushTicker = time.NewTicker(p.accumulator.WindowDuration())
		flushCh = flushTicker.C
		defer flushTicker.Stop()
	}

	for {
		select {
		case <-p.stopCh:
			return nil
		case <-flushCh:
			p.mu.Lock()
			if p.accumulator != nil {
				if result := p.accumulator.Flush(); result != nil {
					if err := p.sendAggResult(result); err != nil {
						p.mu.Unlock()
						return err
					}
				}
			}
			p.mu.Unlock()
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

		// Apply filter - skip if doesn't match
		if !store.MatchesFilter(data, p.config.Filter, p.config.FilterIgnoreCase) {
			// Update lastTimestamp even for filtered items to avoid re-processing
			p.mu.Lock()
			p.lastTimestamp = handle.Timestamp
			p.mu.Unlock()
			continue
		}

		// Aggregation path: feed to accumulator
		if p.accumulator != nil {
			if err := p.feedToAccumulator(handle, data); err != nil {
				return err
			}
			continue
		}

		// Non-aggregation path: send immediately
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

// feedToAccumulator parses data and feeds it to the accumulator,
// sending any completed window result over the WebSocket.
func (p *Pusher) feedToAccumulator(handle *store.ObjectHandle, rawData []byte) error {
	// Expand schema data
	var jsonData []byte
	if p.store.DataType() == store.DataTypeSchema {
		expanded, err := p.store.ExpandData(rawData, 0)
		if err != nil {
			jsonData = rawData
		} else {
			jsonData = expanded
		}
	} else {
		jsonData = rawData
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonData, &parsed); err != nil {
		// Skip unparseable records but advance cursor
		p.mu.Lock()
		p.lastTimestamp = handle.Timestamp
		p.mu.Unlock()
		return nil
	}

	p.mu.Lock()
	result := p.accumulator.Add(handle.Timestamp, parsed)
	p.lastTimestamp = handle.Timestamp
	p.mu.Unlock()

	if result != nil {
		p.mu.Lock()
		err := p.sendAggResult(result)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}

	return nil
}

// sendAggResult sends an aggregation result over the WebSocket. Caller must hold p.mu.
func (p *Pusher) sendAggResult(result *aggregation.AggResult) error {
	if p.conn == nil {
		return nil
	}

	msg := struct {
		Type      string                 `json:"type"`
		Timestamp int64                  `json:"timestamp"`
		Data      map[string]interface{} `json:"data"`
	}{
		Type:      "data",
		Timestamp: result.Timestamp,
		Data:      result.Data,
	}

	if err := p.conn.WriteJSON(msg); err != nil {
		return err
	}
	atomic.AddInt64(&p.messagesSent, 1)
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
