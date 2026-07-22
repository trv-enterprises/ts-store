// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// docker-stats collects Docker container and daemon statistics and writes
// them to ts-store. It talks to the local Docker daemon over its Unix
// socket (/var/run/docker.sock) using raw HTTP — no Docker SDK, so the
// result is a single static binary with zero dependencies.
//
// Two logical streams are produced, each into its own store:
//
//   - docker-containers: one row per running container per tick (20s
//     default). A "container" field distinguishes the series. CPU % is
//     derived from the delta between successive samples (same math as
//     `docker stats`); network and block-IO are stored as raw cumulative
//     counters so rates can be computed at query time without losing data
//     to a missed tick.
//
//   - docker-daemon: one row per daemon tick (120s default) with
//     host-wide totals from /info and /system/df. This ticks slower
//     because those numbers barely move and /system/df is comparatively
//     expensive (it walks the filesystem).
//
// Usage:
//
//	docker-stats -socket /var/run/tsstore/tsstore.sock \
//	             -container-key <key> -daemon-key <key>
//	docker-stats -stdout   # print both streams to stdout for testing
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// ---------------------------------------------------------------------------
// Output records (flattened for schema-based storage, dot-notation names).
// ---------------------------------------------------------------------------

// ContainerStats is one row in the docker-containers store: a single
// container's metrics at one tick. Network and block-IO are cumulative
// counters (raw from Docker), not per-second rates.
type ContainerStats struct {
	Container      string  `json:"container"`
	Image          string  `json:"image,omitempty"`
	State          string  `json:"state,omitempty"`
	CPUPct         float64 `json:"cpu.pct"`
	MemUsed        int64   `json:"mem.used"`
	MemLimit       int64   `json:"mem.limit"`
	MemPct         float64 `json:"mem.pct"`
	NetRxBytes     int64   `json:"net.rx_bytes"`
	NetTxBytes     int64   `json:"net.tx_bytes"`
	BlkioReadBytes int64   `json:"blkio.read_bytes"`
	BlkioWriteByte int64   `json:"blkio.write_bytes"`
	Pids           int32   `json:"pids"`
}

// DaemonStats is one row in the docker-daemon store: host-wide totals.
type DaemonStats struct {
	ContainersRunning int32 `json:"containers.running"`
	ContainersPaused  int32 `json:"containers.paused"`
	ContainersStopped int32 `json:"containers.stopped"`
	ContainersTotal   int32 `json:"containers.total"`
	ImagesTotal       int32 `json:"images.total"`
	DfLayersSize      int64 `json:"df.layers_size"`
	DfImagesSize      int64 `json:"df.images_size"`
	DfContainersSize  int64 `json:"df.containers_size"`
	DfVolumesSize     int64 `json:"df.volumes_size"`
}

// ---------------------------------------------------------------------------
// Docker API client (raw HTTP over the daemon's Unix socket).
// ---------------------------------------------------------------------------

// dockerClient issues requests to the Docker Engine API over its Unix
// socket. The HTTP host is a placeholder ("docker") — the transport
// always dials the socket regardless of URL host.
type dockerClient struct {
	http *http.Client
}

func newDockerClient(socketPath string) *dockerClient {
	return &dockerClient{
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// get issues GET http://docker<path> and decodes the JSON body into out.
func (c *dockerClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- Docker API response shapes (only the fields we consume) ---

type apiContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
}

type apiStats struct {
	CPUStats    apiCPUStats `json:"cpu_stats"`
	PreCPUStats apiCPUStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage int64 `json:"usage"`
		Limit int64 `json:"limit"`
		Stats struct {
			Cache        int64 `json:"cache"`
			InactiveFile int64 `json:"inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
	Networks   map[string]apiNetwork `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []apiBlkioEntry `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current int32 `json:"current"`
	} `json:"pids_stats"`
}

type apiCPUStats struct {
	CPUUsage struct {
		Total int64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemUsage int64 `json:"system_cpu_usage"`
	OnlineCPUs  int32 `json:"online_cpus"`
}

type apiNetwork struct {
	RxBytes int64 `json:"rx_bytes"`
	TxBytes int64 `json:"tx_bytes"`
}

type apiBlkioEntry struct {
	Op    string `json:"op"`
	Value int64  `json:"value"`
}

type apiInfo struct {
	Containers        int32 `json:"Containers"`
	ContainersRunning int32 `json:"ContainersRunning"`
	ContainersPaused  int32 `json:"ContainersPaused"`
	ContainersStopped int32 `json:"ContainersStopped"`
	Images            int32 `json:"Images"`
}

type apiDiskUsage struct {
	LayersSize int64          `json:"LayersSize"`
	Images     []apiSizeEntry `json:"Images"`
	Containers []apiSizeEntry `json:"Containers"`
	Volumes    []apiVolume    `json:"Volumes"`
}

type apiSizeEntry struct {
	Size int64 `json:"Size"`
}

type apiVolume struct {
	UsageData struct {
		Size int64 `json:"Size"`
	} `json:"UsageData"`
}

// listContainers returns running containers.
func (c *dockerClient) listContainers(ctx context.Context) ([]apiContainer, error) {
	var out []apiContainer
	err := c.get(ctx, "/containers/json", &out)
	return out, err
}

// containerStats fetches a single non-streaming stats snapshot.
func (c *dockerClient) containerStats(ctx context.Context, id string) (apiStats, error) {
	var s apiStats
	err := c.get(ctx, "/containers/"+id+"/stats?stream=false", &s)
	return s, err
}

func (c *dockerClient) info(ctx context.Context) (apiInfo, error) {
	var i apiInfo
	err := c.get(ctx, "/info", &i)
	return i, err
}

func (c *dockerClient) diskUsage(ctx context.Context) (apiDiskUsage, error) {
	var d apiDiskUsage
	err := c.get(ctx, "/system/df", &d)
	return d, err
}

// ---------------------------------------------------------------------------
// Metric derivation.
// ---------------------------------------------------------------------------

// toContainerStats converts a raw Docker stats snapshot into our flat row.
// name is the cleaned container name; image/state come from the list call.
func toContainerStats(name, image, state string, s apiStats) ContainerStats {
	cs := ContainerStats{
		Container: name,
		Image:     image,
		State:     state,
		CPUPct:    cpuPercent(s),
		MemUsed:   memUsed(s),
		MemLimit:  s.MemoryStats.Limit,
		Pids:      s.PidsStats.Current,
	}
	if cs.MemLimit > 0 {
		cs.MemPct = round2(float64(cs.MemUsed) / float64(cs.MemLimit) * 100)
	}
	for _, n := range s.Networks {
		cs.NetRxBytes += n.RxBytes
		cs.NetTxBytes += n.TxBytes
	}
	for _, e := range s.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			cs.BlkioReadBytes += e.Value
		case "write":
			cs.BlkioWriteByte += e.Value
		}
	}
	return cs
}

// cpuPercent mirrors the calculation `docker stats` performs: the ratio of
// the container's CPU-time delta to the system CPU-time delta, scaled by
// the number of online CPUs. Because we request a single non-streaming
// snapshot, Docker itself populates precpu_stats from ~1s earlier, so the
// delta is self-contained within one API call.
func cpuPercent(s apiStats) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.Total - s.PreCPUStats.CPUUsage.Total)
	sysDelta := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = 1
	}
	return round2(cpuDelta / sysDelta * cpus * 100)
}

// memUsed subtracts the page cache from reported usage, matching what
// `docker stats` shows as the container's real working set. cgroup v2
// exposes inactive_file; v1 exposes cache. Prefer v2, fall back to v1.
func memUsed(s apiStats) int64 {
	usage := s.MemoryStats.Usage
	if cache := s.MemoryStats.Stats.InactiveFile; cache > 0 {
		return usage - cache
	}
	return usage - s.MemoryStats.Stats.Cache
}

func toDaemonStats(info apiInfo, df apiDiskUsage) DaemonStats {
	d := DaemonStats{
		ContainersRunning: info.ContainersRunning,
		ContainersPaused:  info.ContainersPaused,
		ContainersStopped: info.ContainersStopped,
		ContainersTotal:   info.Containers,
		ImagesTotal:       info.Images,
		DfLayersSize:      df.LayersSize,
	}
	for _, im := range df.Images {
		d.DfImagesSize += im.Size
	}
	for _, ct := range df.Containers {
		d.DfContainersSize += ct.Size
	}
	for _, v := range df.Volumes {
		d.DfVolumesSize += v.UsageData.Size
	}
	return d
}

func cleanName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	// Docker prefixes container names with "/".
	return strings.TrimPrefix(names[0], "/")
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// ---------------------------------------------------------------------------
// ts-store writers.
// ---------------------------------------------------------------------------

// socketWriter authenticates once against the ts-store Unix socket and
// streams one or more JSON lines per tick over a single connection. This
// is why we don't reuse system-stats's one-line-per-connection helper: a
// container tick emits N lines and we want them on one authenticated conn.
type socketWriter struct {
	socketPath string
	store      string
	apiKey     string
}

// writeBatch opens a connection, authenticates, and writes every record
// as its own JSON line. Timestamps are server-assigned (no envelope), so
// the N rows in one tick get monotonically-increasing timestamps in write
// order — satisfying ts-store's strictly-increasing-per-store rule.
func (w *socketWriter) writeBatch(records [][]byte) error {
	conn, err := net.Dial("unix", w.socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "AUTH %s %s\n", w.store, w.apiKey); err != nil {
		return err
	}
	resp, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("auth failed: %s", strings.TrimSpace(resp))
	}

	for _, rec := range records {
		if _, err := fmt.Fprintf(conn, "%s\n", rec); err != nil {
			return err
		}
		resp, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(resp, "OK") {
			return fmt.Errorf("write failed: %s", strings.TrimSpace(resp))
		}
	}
	return nil
}

// httpWriter POSTs each record individually to the REST insert endpoint.
type httpWriter struct {
	baseURL string
	store   string
	apiKey  string
	client  *http.Client
}

func (w *httpWriter) writeBatch(records [][]byte) error {
	url := fmt.Sprintf("%s/api/stores/%s/data", w.baseURL, w.store)
	for _, rec := range records {
		body, err := json.Marshal(map[string]json.RawMessage{"data": rec})
		if err != nil {
			return err
		}
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", w.apiKey)
		resp, err := w.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("unexpected status: %d", resp.StatusCode)
		}
	}
	return nil
}

// writer is the sink abstraction shared by both stores.
type writer interface {
	writeBatch(records [][]byte) error
}

// ---------------------------------------------------------------------------
// Collection loops.
// ---------------------------------------------------------------------------

func marshalAll[T any](items []T) ([][]byte, error) {
	out := make([][]byte, 0, len(items))
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// collectContainers samples every running container once and returns the
// flattened rows. Per-container stats errors are logged and skipped so one
// bad container doesn't drop the whole tick.
func collectContainers(ctx context.Context, dc *dockerClient) ([]ContainerStats, error) {
	containers, err := dc.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]ContainerStats, 0, len(containers))
	for _, c := range containers {
		s, err := dc.containerStats(ctx, c.ID)
		if err != nil {
			log.Printf("Warning: stats for %s: %v", cleanName(c.Names), err)
			continue
		}
		rows = append(rows, toContainerStats(cleanName(c.Names), c.Image, c.State, s))
	}
	return rows, nil
}

func collectDaemon(ctx context.Context, dc *dockerClient) (DaemonStats, error) {
	info, err := dc.info(ctx)
	if err != nil {
		return DaemonStats{}, err
	}
	df, err := dc.diskUsage(ctx)
	if err != nil {
		return DaemonStats{}, err
	}
	return toDaemonStats(info, df), nil
}

func main() {
	var (
		socketPath   = flag.String("socket", "/var/run/tsstore/tsstore.sock", "ts-store Unix socket path")
		httpURL      = flag.String("http", "", "ts-store HTTP URL (use instead of socket)")
		dockerSock   = flag.String("docker-socket", "/var/run/docker.sock", "Docker daemon Unix socket path")
		containerSt  = flag.String("container-store", "docker-containers", "Store name for per-container stats")
		daemonSt     = flag.String("daemon-store", "docker-daemon", "Store name for daemon-wide stats")
		containerKey = flag.String("container-key", "", "API key for the container store (or TSSTORE_CONTAINER_KEY)")
		daemonKey    = flag.String("daemon-key", "", "API key for the daemon store (or TSSTORE_DAEMON_KEY)")
		cInterval    = flag.Int("container-interval", 20, "Container collection interval in seconds")
		dInterval    = flag.Int("daemon-interval", 120, "Daemon collection interval in seconds")
		stdout       = flag.Bool("stdout", false, "Print to stdout instead of writing to ts-store")
		showVersion  = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("docker-stats", Version)
		return
	}

	if *httpURL == "" {
		*httpURL = os.Getenv("TSSTORE_URL")
	}
	if *containerKey == "" {
		*containerKey = os.Getenv("TSSTORE_CONTAINER_KEY")
	}
	if *daemonKey == "" {
		*daemonKey = os.Getenv("TSSTORE_DAEMON_KEY")
	}
	if !*stdout && (*containerKey == "" || *daemonKey == "") {
		log.Fatal("API keys required: set -container-key and -daemon-key (or TSSTORE_CONTAINER_KEY / TSSTORE_DAEMON_KEY)")
	}

	dc := newDockerClient(*dockerSock)
	useHTTP := *httpURL != ""

	// Build the two sinks.
	var containerW, daemonW writer
	switch {
	case *stdout:
		// nil sinks; handled below.
	case useHTTP:
		hc := &http.Client{Timeout: 10 * time.Second}
		containerW = &httpWriter{baseURL: *httpURL, store: *containerSt, apiKey: *containerKey, client: hc}
		daemonW = &httpWriter{baseURL: *httpURL, store: *daemonSt, apiKey: *daemonKey, client: hc}
	default:
		containerW = &socketWriter{socketPath: *socketPath, store: *containerSt, apiKey: *containerKey}
		daemonW = &socketWriter{socketPath: *socketPath, store: *daemonSt, apiKey: *daemonKey}
	}

	log.Printf("docker-stats %s: containers every %ds, daemon every %ds", Version, *cInterval, *dInterval)
	if *stdout {
		log.Printf("Output: stdout")
	} else if useHTTP {
		log.Printf("Output: %s (stores: %s, %s)", *httpURL, *containerSt, *daemonSt)
	} else {
		log.Printf("Output: %s (stores: %s, %s)", *socketPath, *containerSt, *daemonSt)
	}

	cTicker := time.NewTicker(time.Duration(*cInterval) * time.Second)
	dTicker := time.NewTicker(time.Duration(*dInterval) * time.Second)
	defer cTicker.Stop()
	defer dTicker.Stop()

	// Emit one of each immediately so a store isn't empty until the first
	// tick elapses.
	runContainerTick(dc, containerW, *stdout)
	runDaemonTick(dc, daemonW, *stdout)

	for {
		select {
		case <-cTicker.C:
			runContainerTick(dc, containerW, *stdout)
		case <-dTicker.C:
			runDaemonTick(dc, daemonW, *stdout)
		}
	}
}

func runContainerTick(dc *dockerClient, w writer, toStdout bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := collectContainers(ctx, dc)
	if err != nil {
		log.Printf("Warning: container collection failed: %v", err)
		return
	}
	recs, err := marshalAll(rows)
	if err != nil {
		log.Printf("Warning: marshal containers: %v", err)
		return
	}
	if toStdout {
		for _, r := range recs {
			fmt.Println(string(r))
		}
		return
	}
	if len(recs) == 0 {
		return
	}
	if err := w.writeBatch(recs); err != nil {
		log.Printf("Warning: write containers: %v", err)
	}
}

func runDaemonTick(dc *dockerClient, w writer, toStdout bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := collectDaemon(ctx, dc)
	if err != nil {
		log.Printf("Warning: daemon collection failed: %v", err)
		return
	}
	rec, err := json.Marshal(d)
	if err != nil {
		log.Printf("Warning: marshal daemon: %v", err)
		return
	}
	if toStdout {
		fmt.Println(string(rec))
		return
	}
	if err := w.writeBatch([][]byte{rec}); err != nil {
		log.Printf("Warning: write daemon: %v", err)
	}
}
