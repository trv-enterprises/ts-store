// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/pkg/store"
)

// WSWriteRequest represents a message received from the client.
type WSWriteRequest struct {
	Timestamp int64           `json:"timestamp,omitempty"`
	Data      json.RawMessage `json:"data"`
}

// WSWriteResponse represents a response sent to the client.
type WSWriteResponse struct {
	Type      string `json:"type"` // "ack" or "error"
	Timestamp int64  `json:"timestamp,omitempty"`
	BlockNum  uint32 `json:"block_num,omitempty"`
	Size      uint32 `json:"size,omitempty"`
	Message   string `json:"message,omitempty"`
}

// wsWriter handles receiving data from a WebSocket client and storing it.
type wsWriter struct {
	conn    *websocket.Conn
	store   *store.Store
	format  string // "compact" or "full"
	closeCh chan struct{}
}

// newWSWriter creates a new WebSocket writer.
func newWSWriter(conn *websocket.Conn, st *store.Store, format string) *wsWriter {
	if format == "" {
		format = "full"
	}

	return &wsWriter{
		conn:    conn,
		store:   st,
		format:  format,
		closeCh: make(chan struct{}),
	}
}

// run starts the write loop.
func (w *wsWriter) run() {
	defer w.conn.Close()

	for {
		select {
		case <-w.closeCh:
			return
		default:
		}

		// Set read deadline
		w.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		_, message, err := w.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			// Check if it's a timeout
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				// Send ping to keep alive
				if err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
				continue
			}
			w.sendError(err.Error())
			return
		}

		if err := w.processMessage(message); err != nil {
			w.sendError(err.Error())
			// Continue processing - don't disconnect on single message errors
		}
	}
}

// processMessage processes an incoming message and stores it.
func (w *wsWriter) processMessage(message []byte) error {
	var req WSWriteRequest
	if err := json.Unmarshal(message, &req); err != nil {
		return err
	}

	// Use provided timestamp or generate one
	timestamp := req.Timestamp
	if timestamp == 0 {
		timestamp = time.Now().UnixNano()
	}

	// Validate and process data based on store type and format
	var dataBytes []byte
	dataType := w.store.DataType()

	switch dataType {
	case store.DataTypeSchema:
		// For schema stores, validate and possibly compact
		if w.format == "full" {
			// Validate and compact the data
			compacted, err := w.store.ValidateAndCompact(req.Data)
			if err != nil {
				return err
			}
			dataBytes = compacted
		} else {
			// Assume data is already in compact format
			dataBytes = req.Data
		}
	case store.DataTypeJSON:
		// Validate JSON
		var js json.RawMessage
		if err := json.Unmarshal(req.Data, &js); err != nil {
			return err
		}
		dataBytes = req.Data
	default:
		dataBytes = req.Data
	}

	// Store the data
	handle, err := w.store.PutObject(timestamp, dataBytes)
	if err != nil {
		return err
	}

	// Send ack
	return w.sendAck(handle)
}

// sendAck sends an acknowledgment message.
func (w *wsWriter) sendAck(handle *store.ObjectHandle) error {
	resp := WSWriteResponse{
		Type:      "ack",
		Timestamp: handle.Timestamp,
		BlockNum:  handle.BlockNum,
		Size:      handle.Size,
	}
	return w.conn.WriteJSON(resp)
}

// sendError sends an error message.
func (w *wsWriter) sendError(message string) error {
	resp := WSWriteResponse{
		Type:    "error",
		Message: message,
	}
	return w.conn.WriteJSON(resp)
}

// stop stops the writer.
func (w *wsWriter) stop() {
	close(w.closeCh)
}
