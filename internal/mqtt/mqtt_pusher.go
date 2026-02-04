// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package mqtt provides MQTT sink functionality for outbound publishing.
package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/tviviano/ts-store/internal/aggregation"
	"github.com/tviviano/ts-store/internal/duration"
	"github.com/tviviano/ts-store/pkg/store"
)

// Pusher handles MQTT publishing from a store.
type Pusher struct {
	mu        sync.RWMutex
	store     *store.Store
	storeName string
	config    MQTTConnection

	client        mqtt.Client
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

// NewPusher creates a new MQTT pusher.
func NewPusher(st *store.Store, storeName string, config MQTTConnection) *Pusher {
	// Resolve from: -1 means "now"
	startFrom := config.From
	if startFrom == -1 {
		startFrom = time.Now().UnixNano()
	}

	p := &Pusher{
		store:         st,
		storeName:     storeName,
		config:        config,
		status:        "disconnected",
		lastTimestamp: startFrom,
		stopCh:        make(chan struct{}),
	}

	// Try to load persisted cursor if persistence is enabled (overrides startFrom)
	if config.CursorPersistInterval > 0 {
		p.loadCursor()
	}

	// Initialize aggregation if configured
	if config.AggWindow != "" {
		if err := p.initAggregation(); err != nil {
			log.Printf("MQTT sink %s: aggregation init failed: %v (continuing without aggregation)", config.ID, err)
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
		ID:            p.config.ID,
		BrokerURL:     p.config.BrokerURL,
		Topic:         p.config.Topic,
		From:          p.config.From,
		Status:        p.status,
		CreatedAt:     p.config.CreatedAt,
		LastTimestamp:  p.lastTimestamp,
		MessagesSent:  p.messagesSent,
		Errors:        p.errors,
		LastError:     p.lastError,
	}
}

// Start begins the MQTT connection with auto-reconnect.
func (p *Pusher) Start() error {
	p.wg.Add(1)
	go p.runLoop()
	return nil
}

// Stop stops the MQTT connection.
func (p *Pusher) Stop() error {
	close(p.stopCh)
	p.wg.Wait()

	p.mu.Lock()
	// Flush any remaining aggregated data
	if p.accumulator != nil && p.client != nil && p.client.IsConnected() {
		if result := p.accumulator.Flush(); result != nil {
			p.publishAggResult(result)
		}
	}
	if p.client != nil && p.client.IsConnected() {
		p.client.Disconnect(1000)
		p.client = nil
	}
	p.status = "disconnected"
	p.mu.Unlock()

	// Persist cursor on shutdown if enabled
	if p.config.CursorPersistInterval > 0 {
		p.persistCursor()
	}

	return nil
}

// runLoop is the main connection loop with auto-reconnect.
func (p *Pusher) runLoop() {
	defer p.wg.Done()

	retryDelay := time.Second
	maxRetryDelay := 60 * time.Second
	noReconnect := p.config.CursorPersistInterval == -1

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		err := p.connect()
		if err != nil {
			p.setError(err.Error())

			// If no-reconnect mode, stop permanently
			if noReconnect {
				p.mu.Lock()
				p.status = "failed"
				p.mu.Unlock()
				return
			}

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
		if p.client != nil && p.client.IsConnected() {
			p.client.Disconnect(1000)
		}
		p.client = nil
		p.status = "disconnected"
		p.mu.Unlock()

		// If no-reconnect mode, stop permanently after failure
		if noReconnect {
			p.mu.Lock()
			p.status = "failed"
			p.mu.Unlock()
			return
		}

		// Wait before reconnecting
		select {
		case <-p.stopCh:
			return
		case <-time.After(retryDelay):
		}
	}
}

// connect establishes an MQTT connection to the broker.
func (p *Pusher) connect() error {
	p.mu.Lock()
	p.status = "connecting"
	p.mu.Unlock()

	clientID := p.config.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("tsstore-%s-%s", p.storeName, p.config.ID)
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(p.config.BrokerURL)
	opts.SetClientID(clientID)
	opts.SetAutoReconnect(false) // We handle reconnect ourselves
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetWriteTimeout(10 * time.Second)

	if p.config.Username != "" {
		opts.SetUsername(p.config.Username)
	}
	if p.config.Password != "" {
		opts.SetPassword(p.config.Password)
	}

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		log.Printf("MQTT connection lost for %s: %v", p.storeName, err)
		p.setError(err.Error())
	})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.Wait()
	if token.Error() != nil {
		return token.Error()
	}

	p.mu.Lock()
	p.client = client
	p.status = "connected"
	p.lastError = ""
	p.mu.Unlock()

	log.Printf("MQTT connected to %s for store %s", p.config.BrokerURL, p.storeName)

	return nil
}

// pushLoop sends data to the MQTT broker.
func (p *Pusher) pushLoop() error {
	pollInterval := 100 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Set up cursor persistence ticker if enabled
	var persistTicker *time.Ticker
	var persistCh <-chan time.Time
	if p.config.CursorPersistInterval > 0 {
		persistTicker = time.NewTicker(time.Duration(p.config.CursorPersistInterval) * time.Second)
		persistCh = persistTicker.C
		defer persistTicker.Stop()
	}

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
		case <-persistCh:
			p.persistCursor()
		case <-flushCh:
			p.mu.Lock()
			if p.accumulator != nil {
				if result := p.accumulator.Flush(); result != nil {
					if err := p.publishAggResult(result); err != nil {
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
	client := p.client
	p.mu.RUnlock()

	if client == nil || !client.IsConnected() {
		return fmt.Errorf("not connected")
	}

	// Get objects since last timestamp
	var handles []*store.ObjectHandle
	var err error

	if lastTs == 0 {
		// Get oldest objects
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

		// Aggregation path: feed to accumulator
		if p.accumulator != nil {
			if err := p.feedToAccumulator(handle, data); err != nil {
				return err
			}
			continue
		}

		// Non-aggregation path: publish directly
		// Format payload based on store type
		var payload []byte
		if p.store.DataType() == store.DataTypeSchema {
			// Expand schema data to JSON
			expanded, err := p.store.ExpandData(data, 0)
			if err == nil {
				payload = expanded
			} else {
				payload = data
			}
		} else {
			payload = data
		}

		// Wrap with timestamp if configured
		if p.config.IncludeTimestamp {
			msg := struct {
				Timestamp int64           `json:"timestamp"`
				Data      json.RawMessage `json:"data"`
			}{
				Timestamp: handle.Timestamp,
				Data:      payload,
			}
			payload, _ = json.Marshal(msg)
		}

		// Publish with QoS 1 (at least once) and wait for ACK
		token := client.Publish(p.config.Topic, 1, false, payload)
		token.Wait()
		if token.Error() != nil {
			return token.Error()
		}

		// Advance cursor only after confirmed
		p.mu.Lock()
		p.lastTimestamp = handle.Timestamp
		atomic.AddInt64(&p.messagesSent, 1)
		p.mu.Unlock()
	}

	return nil
}

// feedToAccumulator parses data and feeds it to the accumulator,
// publishing any completed window result via MQTT.
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
		err := p.publishAggResult(result)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}

	return nil
}

// publishAggResult publishes an aggregation result via MQTT. Caller must hold p.mu.
func (p *Pusher) publishAggResult(result *aggregation.AggResult) error {
	if p.client == nil || !p.client.IsConnected() {
		return nil
	}

	var payload []byte
	if p.config.IncludeTimestamp {
		msg := struct {
			Timestamp int64                  `json:"timestamp"`
			Data      map[string]interface{} `json:"data"`
		}{
			Timestamp: result.Timestamp,
			Data:      result.Data,
		}
		payload, _ = json.Marshal(msg)
	} else {
		payload, _ = json.Marshal(result.Data)
	}

	token := p.client.Publish(p.config.Topic, 1, false, payload)
	token.Wait()
	if token.Error() != nil {
		return token.Error()
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

// cursorFilePath returns the path to the cursor file.
func (p *Pusher) cursorFilePath() string {
	return filepath.Join(p.store.StorePath(), fmt.Sprintf("mqtt_%s.cursor", p.config.ID))
}

// loadCursor loads the cursor from disk.
func (p *Pusher) loadCursor() {
	data, err := os.ReadFile(p.cursorFilePath())
	if err != nil {
		return
	}

	cursor, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || cursor <= 0 {
		return
	}

	p.mu.Lock()
	p.lastTimestamp = cursor
	p.mu.Unlock()

	log.Printf("MQTT sink %s: loaded cursor %d", p.config.ID, cursor)
}

// persistCursor saves the cursor to disk.
func (p *Pusher) persistCursor() {
	p.mu.RLock()
	cursor := p.lastTimestamp
	p.mu.RUnlock()

	if cursor <= 0 {
		return
	}

	data := []byte(strconv.FormatInt(cursor, 10))
	if err := os.WriteFile(p.cursorFilePath(), data, 0644); err != nil {
		log.Printf("Warning: failed to persist MQTT cursor: %v", err)
	}
}
