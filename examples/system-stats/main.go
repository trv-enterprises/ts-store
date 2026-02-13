// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// system-stats collects system statistics and writes them to ts-store.
// It reads directly from /proc for minimal overhead.
//
// Usage:
//
//	system-stats -socket /var/run/tsstore/tsstore.sock -store system-stats -key <api-key>
//	system-stats -stdout  # Output to stdout for testing
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// SystemStats is flattened for schema-based storage
type SystemStats struct {
	CPUPct              int   `json:"cpu.pct"`
	MemoryTotal         int64 `json:"memory.total"`
	MemoryUsed          int64 `json:"memory.used"`
	MemoryAvailable     int64 `json:"memory.available"`
	MemoryPct           int   `json:"memory.pct"`
	DiskIOReadByteSec   int64 `json:"disk_io.read_bytes_sec"`
	DiskIOWriteByteSec  int64 `json:"disk_io.write_bytes_sec"`
	NetworkRxByteSec    int64 `json:"network.rx_bytes_sec"`
	NetworkTxByteSec    int64 `json:"network.tx_bytes_sec"`
	DiskSpaceTotal      int64 `json:"disk_space.total"`
	DiskSpaceUsed       int64 `json:"disk_space.used"`
	DiskSpaceAvailable  int64 `json:"disk_space.available"`
	DiskSpacePct        int   `json:"disk_space.pct"`
}

// MemoryStats for internal use
type MemoryStats struct {
	Total     int64
	Used      int64
	Available int64
	Pct       int
}

// DiskSpace for internal use
type DiskSpace struct {
	Total     int64
	Used      int64
	Available int64
	Pct       int
}

type cpuRaw struct {
	total int64
	idle  int64
}

type diskIORaw struct {
	readBytes  int64
	writeBytes int64
}

type netIORaw struct {
	rxBytes int64
	txBytes int64
}

func readCPUStats() (cpuRaw, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuRaw{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 8 && fields[0] == "cpu" {
			var user, nice, system, idle, iowait, irq, softirq, steal int64
			user, _ = strconv.ParseInt(fields[1], 10, 64)
			nice, _ = strconv.ParseInt(fields[2], 10, 64)
			system, _ = strconv.ParseInt(fields[3], 10, 64)
			idle, _ = strconv.ParseInt(fields[4], 10, 64)
			iowait, _ = strconv.ParseInt(fields[5], 10, 64)
			irq, _ = strconv.ParseInt(fields[6], 10, 64)
			softirq, _ = strconv.ParseInt(fields[7], 10, 64)
			if len(fields) >= 9 {
				steal, _ = strconv.ParseInt(fields[8], 10, 64)
			}
			total := user + nice + system + idle + iowait + irq + softirq + steal
			return cpuRaw{total: total, idle: idle}, nil
		}
	}
	return cpuRaw{}, fmt.Errorf("failed to parse /proc/stat")
}

func readMemory() (MemoryStats, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemoryStats{}, err
	}
	defer f.Close()

	var total, available int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			val, _ := strconv.ParseInt(fields[1], 10, 64)
			switch {
			case strings.HasPrefix(line, "MemTotal:"):
				total = val * 1024 // Convert KB to bytes
			case strings.HasPrefix(line, "MemAvailable:"):
				available = val * 1024
			}
		}
	}

	used := total - available
	pct := 0
	if total > 0 {
		pct = int(used * 100 / total)
	}

	return MemoryStats{
		Total:     total,
		Used:      used,
		Available: available,
		Pct:       pct,
	}, nil
}

func readDiskIO() (diskIORaw, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return diskIORaw{}, err
	}
	defer f.Close()

	var readSectors, writeSectors int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 14 {
			dev := fields[2]
			// Match sd*, nvme*n*, vd* (whole devices, not partitions)
			if isBlockDevice(dev) {
				rs, _ := strconv.ParseInt(fields[5], 10, 64)  // sectors read
				ws, _ := strconv.ParseInt(fields[9], 10, 64)  // sectors written
				readSectors += rs
				writeSectors += ws
			}
		}
	}

	// Sectors are typically 512 bytes
	return diskIORaw{
		readBytes:  readSectors * 512,
		writeBytes: writeSectors * 512,
	}, nil
}

func isBlockDevice(dev string) bool {
	// Match whole block devices, not partitions
	if strings.HasPrefix(dev, "sd") && len(dev) == 3 {
		return true
	}
	if strings.HasPrefix(dev, "vd") && len(dev) == 3 {
		return true
	}
	if strings.HasPrefix(dev, "nvme") {
		// nvme0n1 but not nvme0n1p1
		if strings.Contains(dev, "n") && !strings.Contains(dev, "p") {
			return true
		}
	}
	return false
}

func readNetIO() (netIORaw, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return netIORaw{}, err
	}
	defer f.Close()

	var rxBytes, txBytes int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, ":") {
			continue
		}
		// Skip loopback
		if strings.Contains(line, "lo:") {
			continue
		}
		// Remove interface name prefix
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) >= 10 {
			rx, _ := strconv.ParseInt(fields[0], 10, 64)
			tx, _ := strconv.ParseInt(fields[8], 10, 64)
			rxBytes += rx
			txBytes += tx
		}
	}

	return netIORaw{rxBytes: rxBytes, txBytes: txBytes}, nil
}

func writeToSocket(socketPath, storeName, apiKey string, data []byte) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Auth first
	fmt.Fprintf(conn, "AUTH %s %s\n", storeName, apiKey)
	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("auth failed: %s", strings.TrimSpace(resp))
	}

	// Write data (just the JSON, no command prefix)
	fmt.Fprintf(conn, "%s\n", string(data))
	resp, err = reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("put failed: %s", strings.TrimSpace(resp))
	}

	return nil
}

func writeToHTTP(httpURL, storeName, apiKey string, data []byte) error {
	url := fmt.Sprintf("%s/api/stores/%s/data", httpURL, storeName)

	// Wrap data in expected format
	body := map[string]json.RawMessage{"data": data}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

func main() {
	var (
		socketPath = flag.String("socket", "/var/run/tsstore/tsstore.sock", "ts-store Unix socket path")
		httpURL    = flag.String("http", "", "ts-store HTTP URL (use instead of socket)")
		storeName  = flag.String("store", "system-stats", "Store name")
		apiKey     = flag.String("key", "", "API key for the store")
		interval   = flag.Int("interval", 20, "Collection interval in seconds")
		stdout     = flag.Bool("stdout", false, "Output to stdout instead of ts-store")
	)
	flag.Parse()

	// Check environment for HTTP URL
	if *httpURL == "" {
		*httpURL = os.Getenv("TSSTORE_URL")
	}

	if !*stdout && *apiKey == "" {
		// Try to read from environment
		*apiKey = os.Getenv("TSSTORE_API_KEY")
		if *apiKey == "" {
			log.Fatal("API key required: use -key flag or set TSSTORE_API_KEY")
		}
	}

	useHTTP := *httpURL != ""

	// Initialize previous values
	cpu1, err := readCPUStats()
	if err != nil {
		log.Fatalf("Failed to read CPU stats: %v", err)
	}
	disk1, err := readDiskIO()
	if err != nil {
		log.Fatalf("Failed to read disk IO: %v", err)
	}
	net1, err := readNetIO()
	if err != nil {
		log.Fatalf("Failed to read network IO: %v", err)
	}

	ticker := time.NewTicker(time.Duration(*interval) * time.Second)
	defer ticker.Stop()

	log.Printf("Collecting system stats every %d seconds", *interval)
	if *stdout {
		log.Printf("Output: stdout")
	} else if useHTTP {
		log.Printf("Output: %s (store: %s)", *httpURL, *storeName)
	} else {
		log.Printf("Output: %s (store: %s)", *socketPath, *storeName)
	}

	for range ticker.C {
		// Read current values
		cpu2, err := readCPUStats()
		if err != nil {
			log.Printf("Warning: failed to read CPU stats: %v", err)
			continue
		}
		disk2, err := readDiskIO()
		if err != nil {
			log.Printf("Warning: failed to read disk IO: %v", err)
			continue
		}
		net2, err := readNetIO()
		if err != nil {
			log.Printf("Warning: failed to read network IO: %v", err)
			continue
		}
		memory, err := readMemory()
		if err != nil {
			log.Printf("Warning: failed to read memory: %v", err)
			continue
		}

		// Calculate CPU percentage
		cpuPct := 0
		totalDelta := cpu2.total - cpu1.total
		idleDelta := cpu2.idle - cpu1.idle
		if totalDelta > 0 {
			cpuPct = int((totalDelta - idleDelta) * 100 / totalDelta)
		}

		// Calculate rates
		intervalSec := int64(*interval)
		diskReadRate := (disk2.readBytes - disk1.readBytes) / intervalSec
		diskWriteRate := (disk2.writeBytes - disk1.writeBytes) / intervalSec
		netRxRate := (net2.rxBytes - net1.rxBytes) / intervalSec
		netTxRate := (net2.txBytes - net1.txBytes) / intervalSec

		// Get disk space
		diskSpace := readDiskSpace()

		stats := SystemStats{
			CPUPct:             cpuPct,
			MemoryTotal:        memory.Total,
			MemoryUsed:         memory.Used,
			MemoryAvailable:    memory.Available,
			MemoryPct:          memory.Pct,
			DiskIOReadByteSec:  diskReadRate,
			DiskIOWriteByteSec: diskWriteRate,
			NetworkRxByteSec:   netRxRate,
			NetworkTxByteSec:   netTxRate,
			DiskSpaceTotal:     diskSpace.Total,
			DiskSpaceUsed:      diskSpace.Used,
			DiskSpaceAvailable: diskSpace.Available,
			DiskSpacePct:       diskSpace.Pct,
		}

		data, err := json.Marshal(stats)
		if err != nil {
			log.Printf("Warning: failed to marshal stats: %v", err)
			continue
		}

		if *stdout {
			fmt.Println(string(data))
		} else if useHTTP {
			if err := writeToHTTP(*httpURL, *storeName, *apiKey, data); err != nil {
				log.Printf("Warning: failed to write to ts-store: %v", err)
			}
		} else {
			if err := writeToSocket(*socketPath, *storeName, *apiKey, data); err != nil {
				log.Printf("Warning: failed to write to ts-store: %v", err)
			}
		}

		// Shift for next iteration
		cpu1 = cpu2
		disk1 = disk2
		net1 = net2
	}
}
