// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package store

import (
	"fmt"
	"testing"
)

func benchStore(b *testing.B, blockSize uint32) *Store {
	b.Helper()
	cfg := DefaultConfig()
	cfg.Name = "bench"
	cfg.Path = b.TempDir()
	cfg.NumBlocks = 200000
	cfg.DataBlockSize = blockSize
	cfg.DataType = DataTypeJSON
	s, err := Create(cfg)
	if err != nil {
		b.Fatalf("Create: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// BenchmarkPutObject measures the write path (payload ~120 B, the size
// class of a system-stats record) at the two block sizes in production.
func BenchmarkPutObject(b *testing.B) {
	payload := []byte(`{"cpu":42.5,"mem":63.1,"temp":51.2,"disk":80.0,"net_rx":123456,"net_tx":654321,"load1":0.42,"uptime":86400}`)
	for _, bs := range []uint32{512, 4096} {
		b.Run(fmt.Sprintf("block%d", bs), func(b *testing.B) {
			s := benchStore(b, bs)
			ts := int64(1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ts++
				if _, err := s.PutObject(ts, payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkGetObject measures the read path over packed blocks.
func BenchmarkGetObject(b *testing.B) {
	payload := []byte(`{"cpu":42.5,"mem":63.1,"temp":51.2,"disk":80.0,"net_rx":123456,"net_tx":654321,"load1":0.42,"uptime":86400}`)
	for _, bs := range []uint32{512, 4096} {
		b.Run(fmt.Sprintf("block%d", bs), func(b *testing.B) {
			s := benchStore(b, bs)
			handles := make([]*ObjectHandle, 0, 1000)
			for i := 0; i < 1000; i++ {
				h, err := s.PutObject(int64(i+1), payload)
				if err != nil {
					b.Fatal(err)
				}
				handles = append(handles, h)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.GetObject(handles[i%len(handles)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
