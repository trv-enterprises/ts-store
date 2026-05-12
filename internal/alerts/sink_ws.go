// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package alerts

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tviviano/ts-store/internal/notify"
)

const wsSinkDialTimeout = 10 * time.Second

// WSSink dispatches alerts over a fresh outbound WebSocket connection per
// alert: dial, send one JSON frame, send a close frame, close. This is
// the right shape for sporadic alerts (an open keep-alive connection is
// wasteful and brittle for low-frequency notifications).
type WSSink struct {
	url     string
	headers http.Header
}

// NewWSSink builds a WS sink. Headers are sent on the upgrade request.
func NewWSSink(url string, headers map[string]string) *WSSink {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &WSSink{url: url, headers: h}
}

func (s *WSSink) Send(alert notify.Alert) error {
	dialer := websocket.Dialer{HandshakeTimeout: wsSinkDialTimeout}
	conn, _, err := dialer.Dial(s.url, s.headers)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", s.url, err)
	}
	defer conn.Close()

	// Write deadline matches the dial timeout to bound total Send latency.
	if err := conn.SetWriteDeadline(time.Now().Add(wsSinkDialTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}

	msg := struct {
		Type      string       `json:"type"`
		Timestamp int64        `json:"timestamp"`
		Alert     notify.Alert `json:"alert"`
	}{
		Type:      "alert",
		Timestamp: alert.Timestamp,
		Alert:     alert,
	}

	if err := conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("ws write: %w", err)
	}

	// Best-effort clean close so the peer sees a normal disconnect.
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	return nil
}

// Close is a no-op since each Send opens and closes its own connection.
func (s *WSSink) Close() error { return nil }
