// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package handlers

import (
	"encoding/json"
	"sync"
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

// wsWriteTimeout bounds each outbound write. Also load-bearing: the HTTP
// server's WriteTimeout arms an absolute deadline on the underlying conn at
// request start, which the hijacked WS connection inherits — without a fresh
// per-write deadline every ack fails once that elapses (~30s into a session).
const wsWriteTimeout = 10 * time.Second

// wsMaxMessageBytes bounds a single inbound WS frame; without it one frame
// can buffer gigabytes (issue #30). Var (not const) so tests can exercise
// the limit quickly.
var wsMaxMessageBytes int64 = 4 << 20

// wsWriter handles receiving data from a WebSocket client and storing it.
type wsWriter struct {
	conn     *websocket.Conn
	store    *store.Store
	format   string // "compact" or "full"
	closeCh  chan struct{}
	stopOnce sync.Once
}

// newWSWriter creates a new WebSocket writer.
func newWSWriter(conn *websocket.Conn, st *store.Store, format string) *wsWriter {
	if format == "" {
		format = "full"
	}

	conn.SetReadLimit(wsMaxMessageBytes)
	return &wsWriter{
		conn:    conn,
		store:   st,
		format:  format,
		closeCh: make(chan struct{}),
	}
}

// stop asks the write loop to exit: it signals closeCh, tells the client
// we're going away, and expires the pending read so run() doesn't sit in
// ReadMessage for up to its 60s deadline. Safe to call more than once and
// concurrently with run(); used on server shutdown.
func (w *wsWriter) stop() {
	w.stopOnce.Do(func() {
		close(w.closeCh)
		_ = w.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			time.Now().Add(time.Second))
		_ = w.conn.SetReadDeadline(time.Now())
	})
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
				// A stop() expires the read deadline on purpose — exit
				// instead of treating it as an idle keep-alive timeout.
				select {
				case <-w.closeCh:
					return
				default:
				}
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
			// format=compact: the payload claims to already be in
			// compact (index-keyed) form. Validate it — HTTP has no
			// compact write path and always validates+compacts, so
			// without this a WS client could insert arbitrary bytes
			// that reads then fail to expand (issue #41).
			if err := w.store.ValidateCompact(req.Data); err != nil {
				return err
			}
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
	w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.conn.WriteJSON(resp)
}

// sendError sends an error message.
func (w *wsWriter) sendError(message string) error {
	resp := WSWriteResponse{
		Type:    "error",
		Message: message,
	}
	w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.conn.WriteJSON(resp)
}
