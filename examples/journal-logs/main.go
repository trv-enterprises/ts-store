// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// journal-logs streams journalctl output to ts-store via Unix socket.
//
// Usage:
//
//	journal-logs -socket /var/run/tsstore/tsstore.sock -store journal-logs -key <api-key>
//	journal-logs -stdout  # Output to stdout for testing
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// JournalEntry represents a parsed journalctl JSON entry
type JournalEntry struct {
	Timestamp         string `json:"__REALTIME_TIMESTAMP,omitempty"`
	Hostname          string `json:"_HOSTNAME,omitempty"`
	Unit              string `json:"_SYSTEMD_UNIT,omitempty"`
	SyslogIdentifier  string `json:"SYSLOG_IDENTIFIER,omitempty"`
	Message           string `json:"MESSAGE,omitempty"`
	Priority          string `json:"PRIORITY,omitempty"`
	PID               string `json:"_PID,omitempty"`
	UID               string `json:"_UID,omitempty"`
	Comm              string `json:"_COMM,omitempty"`
}

// LogEntry is the simplified structure we store
type LogEntry struct {
	Time     string `json:"time"`
	Host     string `json:"host,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Ident    string `json:"ident,omitempty"`
	Message  string `json:"msg"`
	Priority int    `json:"pri,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

func main() {
	var (
		socketPath = flag.String("socket", "/var/run/tsstore/tsstore.sock", "ts-store Unix socket path")
		httpURL    = flag.String("http", "", "ts-store HTTP URL (use instead of socket)")
		storeName  = flag.String("store", "journal-logs", "Store name")
		apiKey     = flag.String("key", "", "API key for the store")
		stdout     = flag.Bool("stdout", false, "Output to stdout instead of ts-store")
		since      = flag.String("since", "", "Start reading from this time (e.g., '1 hour ago', 'today')")
		units      = flag.String("units", "", "Comma-separated list of units to filter (e.g., 'sshd,nginx')")
		priority   = flag.String("priority", "", "Maximum priority level (0=emerg to 7=debug)")
	)
	flag.Parse()

	// Check environment for HTTP URL
	if *httpURL == "" {
		*httpURL = os.Getenv("TSSTORE_URL")
	}

	if !*stdout && *apiKey == "" {
		*apiKey = os.Getenv("TSSTORE_API_KEY")
		if *apiKey == "" {
			log.Fatal("API key required: use -key flag or set TSSTORE_API_KEY")
		}
	}

	useHTTP := *httpURL != ""

	log.Printf("Starting journal log stream")
	if *stdout {
		log.Printf("Output: stdout")
	} else if useHTTP {
		log.Printf("Output: %s (store: %s)", *httpURL, *storeName)
	} else {
		log.Printf("Output: %s (store: %s)", *socketPath, *storeName)
	}

	// Build journalctl command
	args := []string{"-f", "-o", "json", "--no-pager"}
	if *since != "" {
		args = append(args, "--since", *since)
	}
	if *units != "" {
		for _, unit := range strings.Split(*units, ",") {
			unit = strings.TrimSpace(unit)
			if unit != "" {
				args = append(args, "-u", unit)
			}
		}
	}
	if *priority != "" {
		args = append(args, "-p", *priority)
	}

	log.Printf("journalctl args: %v", args)

	// Start journalctl
	cmd := exec.Command("journalctl", args...)
	cmdStdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start journalctl: %v", err)
	}

	// Process output
	reader := bufio.NewReader(cmdStdout)
	var conn net.Conn
	var connWriter *bufio.Writer
	var connReader *bufio.Reader

	// Connect to socket if not stdout mode
	if !*stdout && !useHTTP {
		conn, connWriter, connReader, err = connectAndAuth(*socketPath, *storeName, *apiKey)
		if err != nil {
			log.Fatalf("Failed to connect to ts-store: %v", err)
		}
		defer conn.Close()
	}

	var totalSent, totalErrors int64
	lastStatsTime := time.Now()

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				log.Printf("journalctl ended")
				break
			}
			log.Printf("Error reading journalctl: %v", err)
			continue
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse journalctl JSON
		var entry JournalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			log.Printf("Failed to parse journal entry: %v", err)
			continue
		}

		// Convert to our simplified format
		logEntry := convertEntry(entry)

		data, err := json.Marshal(logEntry)
		if err != nil {
			log.Printf("Failed to marshal log entry: %v", err)
			continue
		}

		if *stdout {
			fmt.Println(string(data))
			totalSent++
		} else if useHTTP {
			if err := writeToHTTP(*httpURL, *storeName, *apiKey, data); err != nil {
				totalErrors++
				if totalErrors%100 == 1 {
					log.Printf("Warning: failed to write to ts-store: %v", err)
				}
			} else {
				totalSent++
			}
		} else {
			if err := writeToSocket(connWriter, connReader, data); err != nil {
				totalErrors++
				if totalErrors%100 == 1 {
					log.Printf("Warning: failed to write to ts-store: %v", err)
				}
				// Try to reconnect
				conn.Close()
				conn, connWriter, connReader, err = connectAndAuth(*socketPath, *storeName, *apiKey)
				if err != nil {
					log.Printf("Failed to reconnect: %v", err)
					time.Sleep(5 * time.Second)
				}
			} else {
				totalSent++
			}
		}

		// Log stats every 60 seconds
		if time.Since(lastStatsTime) > 60*time.Second {
			log.Printf("Stats: sent=%d, errors=%d", totalSent, totalErrors)
			lastStatsTime = time.Now()
		}
	}

	cmd.Wait()
}

func convertEntry(entry JournalEntry) LogEntry {
	// Parse timestamp (microseconds since epoch)
	var timeStr string
	if entry.Timestamp != "" {
		var usec int64
		fmt.Sscanf(entry.Timestamp, "%d", &usec)
		t := time.UnixMicro(usec)
		timeStr = t.Format(time.RFC3339)
	} else {
		timeStr = time.Now().Format(time.RFC3339)
	}

	// Parse priority
	var pri int
	if entry.Priority != "" {
		fmt.Sscanf(entry.Priority, "%d", &pri)
	}

	// Parse PID
	var pid int
	if entry.PID != "" {
		fmt.Sscanf(entry.PID, "%d", &pid)
	}

	// Use syslog identifier or command name
	ident := entry.SyslogIdentifier
	if ident == "" {
		ident = entry.Comm
	}

	return LogEntry{
		Time:     timeStr,
		Host:     entry.Hostname,
		Unit:     entry.Unit,
		Ident:    ident,
		Message:  entry.Message,
		Priority: pri,
		PID:      pid,
	}
}

func connectAndAuth(socketPath, storeName, apiKey string) (net.Conn, *bufio.Writer, *bufio.Reader, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, nil, nil, err
	}

	writer := bufio.NewWriter(conn)
	reader := bufio.NewReader(conn)

	// Auth
	fmt.Fprintf(writer, "AUTH %s %s\n", storeName, apiKey)
	writer.Flush()

	resp, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, nil, nil, err
	}
	if !strings.HasPrefix(resp, "OK") {
		conn.Close()
		return nil, nil, nil, fmt.Errorf("auth failed: %s", strings.TrimSpace(resp))
	}

	return conn, writer, reader, nil
}

func writeToSocket(writer *bufio.Writer, reader *bufio.Reader, data []byte) error {
	fmt.Fprintf(writer, "%s\n", string(data))
	writer.Flush()

	resp, err := reader.ReadString('\n')
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
