// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/pkg/store"
)

// WSReadMessage represents a message sent to the client.
type WSReadMessage struct {
	Type      string `json:"type"` // "data", "caught_up", "error"
	Timestamp int64  `json:"timestamp,omitempty"`
	BlockNum  uint32 `json:"block_num,omitempty"`
	Size      uint32 `json:"size,omitempty"`
	Data      any    `json:"data,omitempty"`
	Message   string `json:"message,omitempty"`
}

// wsReader handles streaming data to a WebSocket client.
type wsReader struct {
	conn     *websocket.Conn
	store    *store.Store
	from     int64  // Start timestamp (0 = from beginning, -1 = now)
	format   string // "compact" or "full"
	closeCh  chan struct{}
	lastSent int64
	caughtUp bool
}

// newWSReader creates a new WebSocket reader.
func newWSReader(conn *websocket.Conn, st *store.Store, fromStr, format string) (*wsReader, error) {
	var from int64 = -1 // Default to "now"

	if fromStr != "" && fromStr != "now" {
		parsed, err := strconv.ParseInt(fromStr, 10, 64)
		if err != nil {
			return nil, err
		}
		from = parsed
	}

	if format == "" {
		format = "full"
	}

	return &wsReader{
		conn:    conn,
		store:   st,
		from:    from,
		format:  format,
		closeCh: make(chan struct{}),
	}, nil
}

// run starts the read loop.
func (r *wsReader) run() {
	defer r.conn.Close()

	// Start a goroutine to handle incoming messages (pings, close, etc.)
	go r.handleIncoming()

	// If from == -1 (now), start from the current time
	if r.from == -1 {
		r.from = time.Now().UnixNano()
		r.lastSent = r.from
		r.caughtUp = true

		// Send caught_up immediately since we're starting from now
		r.sendCaughtUp()
	} else {
		// Send historical data first
		if err := r.sendHistorical(); err != nil {
			r.sendError(err.Error())
			return
		}
	}

	// Enter the live streaming loop
	r.streamLive()
}

// handleIncoming handles incoming WebSocket messages (pings, close frames, etc.).
func (r *wsReader) handleIncoming() {
	defer close(r.closeCh)

	for {
		_, _, err := r.conn.ReadMessage()
		if err != nil {
			return
		}
		// Ignore incoming messages - this is a read-only stream
	}
}

// sendHistorical sends all data from the "from" timestamp up to now.
func (r *wsReader) sendHistorical() error {
	endTime := time.Now().UnixNano()

	// Get objects in range
	handles, err := r.store.GetObjectsInRange(r.from, endTime, 0) // 0 = no limit
	if err != nil {
		return err
	}

	for _, handle := range handles {
		select {
		case <-r.closeCh:
			return nil
		default:
		}

		data, err := r.store.GetObject(handle)
		if err != nil {
			continue
		}

		if err := r.sendData(handle, data); err != nil {
			return err
		}

		r.lastSent = handle.Timestamp
	}

	// Send caught_up message
	r.caughtUp = true
	return r.sendCaughtUp()
}

// streamLive streams new data as it arrives.
func (r *wsReader) streamLive() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.closeCh:
			return
		case <-ticker.C:
			r.sendNewData()
		}
	}
}

// sendNewData sends any data that arrived since lastSent.
func (r *wsReader) sendNewData() {
	endTime := time.Now().UnixNano()

	handles, err := r.store.GetObjectsInRange(r.lastSent+1, endTime, 100)
	if err != nil {
		return
	}

	for _, handle := range handles {
		select {
		case <-r.closeCh:
			return
		default:
		}

		data, err := r.store.GetObject(handle)
		if err != nil {
			continue
		}

		if err := r.sendData(handle, data); err != nil {
			return
		}

		r.lastSent = handle.Timestamp
	}
}

// sendData sends a single data message.
func (r *wsReader) sendData(handle *store.ObjectHandle, data []byte) error {
	// Format the data
	var payload any
	dataType := r.store.DataType()

	switch dataType {
	case store.DataTypeBinary:
		// Send as base64
		payload = data
	case store.DataTypeText:
		payload = string(data)
	case store.DataTypeJSON:
		payload = json.RawMessage(data)
	case store.DataTypeSchema:
		if r.format == "full" {
			expanded, err := r.store.ExpandData(data, 0)
			if err == nil {
				payload = json.RawMessage(expanded)
			} else {
				payload = json.RawMessage(data)
			}
		} else {
			payload = json.RawMessage(data)
		}
	default:
		payload = json.RawMessage(data)
	}

	msg := WSReadMessage{
		Type:      "data",
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
		Data:      payload,
	}

	return r.conn.WriteJSON(msg)
}

// sendCaughtUp sends the caught_up message.
func (r *wsReader) sendCaughtUp() error {
	msg := WSReadMessage{
		Type: "caught_up",
	}
	return r.conn.WriteJSON(msg)
}

// sendError sends an error message.
func (r *wsReader) sendError(message string) error {
	msg := WSReadMessage{
		Type:    "error",
		Message: message,
	}
	return r.conn.WriteJSON(msg)
}
