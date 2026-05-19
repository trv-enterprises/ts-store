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
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// SystemStats is flattened for schema-based storage.
// Temperature fields are emitted as omitempty so hosts without a given
// sensor (e.g. no NVMe, no NIC temp) simply omit the field rather than
// reporting a misleading 0 °C.
type SystemStats struct {
	CPUPct             int     `json:"cpu.pct"`
	MemoryTotal        int64   `json:"memory.total"`
	MemoryUsed         int64   `json:"memory.used"`
	MemoryAvailable    int64   `json:"memory.available"`
	MemoryPct          int     `json:"memory.pct"`
	DiskIOReadByteSec  int64   `json:"disk_io.read_bytes_sec"`
	DiskIOWriteByteSec int64   `json:"disk_io.write_bytes_sec"`
	NetworkRxByteSec   int64   `json:"network.rx_bytes_sec"`
	NetworkTxByteSec   int64   `json:"network.tx_bytes_sec"`
	DiskSpaceTotal     int64   `json:"disk_space.total"`
	DiskSpaceUsed      int64   `json:"disk_space.used"`
	DiskSpaceAvailable int64   `json:"disk_space.available"`
	DiskSpacePct       int     `json:"disk_space.pct"`
	SwapTotal          int64   `json:"swap.total"`
	SwapUsed           int64   `json:"swap.used"`
	SwapPct            int     `json:"swap.pct"`
	UptimeSec          int64   `json:"uptime.sec"`
	TempCPUPackageC    float64 `json:"temp.cpu_package_c,omitempty"`
	TempCPUMaxCoreC    float64 `json:"temp.cpu_max_core_c,omitempty"`
	TempNVMeC          float64 `json:"temp.nvme_c,omitempty"`
	TempPCHC           float64 `json:"temp.pch_c,omitempty"`
	TempNICC           float64 `json:"temp.nic_c,omitempty"`
}

// MemoryStats for internal use
type MemoryStats struct {
	Total     int64
	Used      int64
	Available int64
	Pct       int
	SwapTotal int64
	SwapUsed  int64
	SwapPct   int
}

// Temps for internal use. Zero-valued fields mean "sensor not present"
// — readers leave them at 0 and the JSON tag emits omitempty.
type Temps struct {
	CPUPackageC float64
	CPUMaxCoreC float64
	NVMeC       float64
	PCHC        float64
	NICC        float64
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

	var total, available, swapTotal, swapFree int64
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
			case strings.HasPrefix(line, "SwapTotal:"):
				swapTotal = val * 1024
			case strings.HasPrefix(line, "SwapFree:"):
				swapFree = val * 1024
			}
		}
	}

	used := total - available
	pct := 0
	if total > 0 {
		pct = int(used * 100 / total)
	}

	swapUsed := swapTotal - swapFree
	swapPct := 0
	if swapTotal > 0 {
		swapPct = int(swapUsed * 100 / swapTotal)
	}

	return MemoryStats{
		Total:     total,
		Used:      used,
		Available: available,
		Pct:       pct,
		SwapTotal: swapTotal,
		SwapUsed:  swapUsed,
		SwapPct:   swapPct,
	}, nil
}

// readUptime returns the system uptime in seconds. /proc/uptime is two
// floats; we only need the first.
func readUptime() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return 0, fmt.Errorf("failed to parse /proc/uptime")
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(up), nil
}

// readTemps walks /sys/class/hwmon and picks out the sensors we care
// about. Values in *_input files are milli-degrees Celsius. Hosts that
// don't have a given chip simply return zero for that field, and the
// JSON tag omits it. The "max core" reading is the hottest core
// reported by the coretemp chip (excluding the package summary).
func readTemps() Temps {
	var t Temps
	matches, err := filepath.Glob("/sys/class/hwmon/hwmon*")
	if err != nil {
		return t
	}
	for _, dir := range matches {
		name := strings.TrimSpace(readFile(filepath.Join(dir, "name")))
		switch name {
		case "coretemp":
			pkg, maxCore := readCoretemp(dir)
			t.CPUPackageC = pkg
			t.CPUMaxCoreC = maxCore
		case "nvme":
			t.NVMeC = readTempInput(filepath.Join(dir, "temp1_input"))
		case "pch_haswell", "pch_skylake", "pch_cannonlake", "pch_lewisburg":
			t.PCHC = readTempInput(filepath.Join(dir, "temp1_input"))
		case "i350bb", "ixgbe", "i40e", "ice":
			t.NICC = readTempInput(filepath.Join(dir, "temp1_input"))
		}
	}
	return t
}

// readCoretemp returns (package, hottest-core) in Celsius from a
// coretemp hwmon dir. Package id is identified by its temp*_label
// starting with "Package"; cores by labels starting with "Core ".
func readCoretemp(dir string) (float64, float64) {
	var pkg, maxCore float64
	inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
	for _, in := range inputs {
		label := strings.TrimSpace(readFile(strings.TrimSuffix(in, "_input") + "_label"))
		val := readTempInput(in)
		switch {
		case strings.HasPrefix(label, "Package"):
			pkg = val
		case strings.HasPrefix(label, "Core "):
			if val > maxCore {
				maxCore = val
			}
		}
	}
	return pkg, maxCore
}

// readTempInput reads a hwmon temp*_input file (milli-°C) and returns
// degrees Celsius. Missing or unparseable files return 0.
func readTempInput(path string) float64 {
	s := strings.TrimSpace(readFile(path))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return float64(v) / 1000.0
}

// readFile is a small helper that returns "" on error rather than
// forcing every caller to handle the missing-file case.
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
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
		socketPath  = flag.String("socket", "/var/run/tsstore/tsstore.sock", "ts-store Unix socket path")
		httpURL     = flag.String("http", "", "ts-store HTTP URL (use instead of socket)")
		storeName   = flag.String("store", "system-stats", "Store name")
		apiKey      = flag.String("key", "", "API key for the store")
		interval    = flag.Int("interval", 20, "Collection interval in seconds")
		stdout      = flag.Bool("stdout", false, "Output to stdout instead of ts-store")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("system-stats", Version)
		return
	}

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

		uptime, err := readUptime()
		if err != nil {
			log.Printf("Warning: failed to read uptime: %v", err)
		}

		temps := readTemps()

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
			SwapTotal:          memory.SwapTotal,
			SwapUsed:           memory.SwapUsed,
			SwapPct:            memory.SwapPct,
			UptimeSec:          uptime,
			TempCPUPackageC:    temps.CPUPackageC,
			TempCPUMaxCoreC:    temps.CPUMaxCoreC,
			TempNVMeC:          temps.NVMeC,
			TempPCHC:           temps.PCHC,
			TempNICC:           temps.NICC,
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
