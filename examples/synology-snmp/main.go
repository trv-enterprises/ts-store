// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

// synology-snmp collects Synology DiskStation health metrics over SNMPv3
// and writes them to ts-store. It polls the NAS remotely, so nothing is
// installed on the DSM box itself — the collector runs anywhere with
// network reach (in this deployment, the services LXC).
//
// Why SNMP and not DSM's JSON API: the DSM web API gates almost every
// useful read behind an account in the `administrators` group. Verified on
// DSM 7.3 — a non-admin account gets error 105 for system utilization,
// services, and storage, and a silently FILTERED package list (5 of 15
// rows, no error). SNMPv3 needs no DSM account at all, and returns strictly
// more: per-disk SMART health, RAID/volume state and UPS status.
//
// Three logical streams are produced, each into its own store:
//
//   - <prefix>-disks:   one row per physical disk per tick, keyed by the
//     "disk" field (SNMP tables are flat OID columns, so a table walk maps
//     to TALL rows — the same shape docker-stats uses for containers).
//   - <prefix>-volumes: one row per volume / storage pool per tick, keyed
//     by "volume".
//   - <prefix>-system:  one WIDE row per tick — chassis health, fans, CPU
//     load, memory and UPS.
//
// Enum fields are stored as RAW INTEGERS, never decoded strings. Synology
// extends these enums between releases without reissuing the MIB bundle
// (verified: the 2022 MIB caps raidStatus at Integer32(1..12) while the
// 2025 guide documents 20 states, including the SHR-specific 18 and 20).
// A string field would need a retype on every such addition, and under
// ts-store's schema rules a retype means recreating the store and losing
// history. Integers absorb the drift; decoding is a read-time concern.
//
// Usage:
//
//	synology-snmp -target 10.0.0.5 -user monitor \
//	              -auth-pass <p> -priv-pass <p> \
//	              -http http://ts-store:21080 -key <key>
//	synology-snmp -target 10.0.0.5 ... -stdout   # print, don't write
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
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// ---------------------------------------------------------------------------
// OID map. Mirrors trv-homelab/edge/synology-snmp/oid-map.yml — see that file
// and mibs/README.md for provenance of every column and enum.
// ---------------------------------------------------------------------------

const (
	oidDiskTable   = ".1.3.6.1.4.1.6574.2.1.1"
	oidVolumeTable = ".1.3.6.1.4.1.6574.3.1.1"
)

// Disk table columns (SYNOLOGY-DISK-MIB).
const (
	colDiskID     = 2
	colDiskModel  = 3
	colDiskType   = 4
	colDiskStatus = 5
	colDiskTemp   = 6 // Celsius, stated explicitly in the MIB
	colDiskRole   = 7
	colDiskHealth = 13 // DSM 7.1+; populated over SNMP even where the JSON API returns null
)

// Volume/RAID table columns (SYNOLOGY-RAID-MIB).
const (
	colVolName     = 2
	colVolStatus   = 3
	colVolFree     = 4 // Counter64, bytes
	colVolTotal    = 5 // Counter64, bytes
	colVolHotspare = 6
)

// scalarOID is one field in the wide system row.
type scalarOID struct {
	oid   string
	field string
	kind  string // "int", "float", "string", "load" (UCD laLoadInt, x100)
}

var systemScalars = []scalarOID{
	{".1.3.6.1.4.1.6574.1.1.0", "system_status", "int"},
	{".1.3.6.1.4.1.6574.1.2.0", "temp_c", "int"},
	{".1.3.6.1.4.1.6574.1.3.0", "power_status", "int"},
	{".1.3.6.1.4.1.6574.1.4.1.0", "fan_system", "int"},
	{".1.3.6.1.4.1.6574.1.4.2.0", "fan_cpu", "int"},
	{".1.3.6.1.4.1.6574.1.5.4.0", "upgrade_avail", "int"},
	// UPS — DisplayString status (e.g. "OL"), plus two Floats.
	{".1.3.6.1.4.1.6574.4.2.1.0", "ups_status", "string"},
	{".1.3.6.1.4.1.6574.4.2.12.1.0", "ups_load_pct", "float"},
	{".1.3.6.1.4.1.6574.4.3.1.1.0", "ups_battery_pct", "float"},
	// Standard MIBs. laLoadInt is load average x100 as an integer.
	{".1.3.6.1.4.1.2021.10.1.5.1", "load1", "load"},
	{".1.3.6.1.4.1.2021.10.1.5.2", "load5", "load"},
	{".1.3.6.1.4.1.2021.10.1.5.3", "load15", "load"},
	{".1.3.6.1.4.1.2021.4.5.0", "mem_total_kb", "int"},
	{".1.3.6.1.4.1.2021.4.6.0", "mem_avail_kb", "int"},
	{".1.3.6.1.4.1.2021.4.15.0", "mem_cached_kb", "int"},
}

// ---------------------------------------------------------------------------
// Output records. Flat typed fields only — no nested arrays anywhere, which
// is what ts-store schema stores accept.
// ---------------------------------------------------------------------------

// DiskRow is one row in the disks store: a single physical disk at one tick.
// Status and Health are raw enum integers (see the file header).
type DiskRow struct {
	Disk     string `json:"disk"`
	Model    string `json:"model,omitempty"`
	DiskType string `json:"disk_type,omitempty"`
	Status   int    `json:"status"`
	TempC    int    `json:"temp_c"`
	Role     string `json:"role,omitempty"`
	Health   int    `json:"health"`
}

// VolumeRow is one row in the volumes store: a volume or storage pool.
type VolumeRow struct {
	Volume      string `json:"volume"`
	Status      int    `json:"status"`
	FreeBytes   int64  `json:"free_bytes"`
	TotalBytes  int64  `json:"total_bytes"`
	HotspareCnt int    `json:"hotspare_cnt"`
}

// ---------------------------------------------------------------------------
// SNMP client.
// ---------------------------------------------------------------------------

// newSNMP builds an SNMPv3 authPriv client. SHA/AES are the only combination
// offered: MD5 and DES are both broken, and there is no reason to make a
// weaker posture reachable by config.
func newSNMP(target string, port uint16, user, authPass, privPass string, timeout time.Duration) *gosnmp.GoSNMP {
	return &gosnmp.GoSNMP{
		Target:        target,
		Port:          port,
		Version:       gosnmp.Version3,
		Timeout:       timeout,
		Retries:       1,
		SecurityModel: gosnmp.UserSecurityModel,
		MsgFlags:      gosnmp.AuthPriv,
		SecurityParameters: &gosnmp.UsmSecurityParameters{
			UserName:                 user,
			AuthenticationProtocol:   gosnmp.SHA,
			AuthenticationPassphrase: authPass,
			PrivacyProtocol:          gosnmp.AES,
			PrivacyPassphrase:        privPass,
		},
	}
}

// walkColumn walks one table column and returns index -> value, where the
// index is the trailing OID element. SNMP tables have no inherent row
// object: a "row" is the set of columns sharing an index, which is why the
// collector joins columns by index rather than parsing a nested structure.
func walkColumn(s *gosnmp.GoSNMP, base string, col int) (map[string]gosnmp.SnmpPDU, error) {
	out := map[string]gosnmp.SnmpPDU{}
	oid := fmt.Sprintf("%s.%d", base, col)
	err := s.Walk(oid, func(pdu gosnmp.SnmpPDU) error {
		idx := strings.TrimPrefix(pdu.Name, oid+".")
		out[idx] = pdu
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", oid, err)
	}
	return out, nil
}

// asInt coerces a PDU to int, tolerating the several integer types SNMP
// uses (Integer, Gauge32, Counter32/64, TimeTicks).
func asInt(pdu gosnmp.SnmpPDU, ok bool) int {
	if !ok || pdu.Value == nil {
		return 0
	}
	return int(gosnmp.ToBigInt(pdu.Value).Int64())
}

func asInt64(pdu gosnmp.SnmpPDU, ok bool) int64 {
	if !ok || pdu.Value == nil {
		return 0
	}
	return gosnmp.ToBigInt(pdu.Value).Int64()
}

// asString coerces a PDU to string. OctetString arrives as []byte.
func asString(pdu gosnmp.SnmpPDU, ok bool) string {
	if !ok || pdu.Value == nil {
		return ""
	}
	if b, isBytes := pdu.Value.([]byte); isBytes {
		return strings.TrimSpace(string(b))
	}
	return strings.TrimSpace(fmt.Sprintf("%v", pdu.Value))
}

// ---------------------------------------------------------------------------
// Collection.
// ---------------------------------------------------------------------------

func collectDisks(s *gosnmp.GoSNMP) ([]DiskRow, error) {
	cols := map[int]map[string]gosnmp.SnmpPDU{}
	for _, c := range []int{colDiskID, colDiskModel, colDiskType, colDiskStatus, colDiskTemp, colDiskRole, colDiskHealth} {
		m, err := walkColumn(s, oidDiskTable, c)
		if err != nil {
			// colDiskHealth is DSM 7.1+; tolerate its absence on older DSM
			// rather than failing the whole tick.
			if c == colDiskHealth {
				cols[c] = map[string]gosnmp.SnmpPDU{}
				continue
			}
			return nil, err
		}
		cols[c] = m
	}

	rows := make([]DiskRow, 0, len(cols[colDiskID]))
	for idx := range cols[colDiskID] {
		name := asString(cols[colDiskID][idx], true)
		if name == "" {
			continue
		}
		h, hOK := cols[colDiskHealth][idx]
		rows = append(rows, DiskRow{
			Disk:     name,
			Model:    asString(cols[colDiskModel][idx], true),
			DiskType: asString(cols[colDiskType][idx], true),
			Status:   asInt(cols[colDiskStatus][idx], true),
			TempC:    asInt(cols[colDiskTemp][idx], true),
			Role:     asString(cols[colDiskRole][idx], true),
			Health:   asInt(h, hOK),
		})
	}
	return rows, nil
}

func collectVolumes(s *gosnmp.GoSNMP) ([]VolumeRow, error) {
	cols := map[int]map[string]gosnmp.SnmpPDU{}
	for _, c := range []int{colVolName, colVolStatus, colVolFree, colVolTotal, colVolHotspare} {
		m, err := walkColumn(s, oidVolumeTable, c)
		if err != nil {
			return nil, err
		}
		cols[c] = m
	}

	rows := make([]VolumeRow, 0, len(cols[colVolName]))
	for idx := range cols[colVolName] {
		name := asString(cols[colVolName][idx], true)
		if name == "" {
			continue
		}
		rows = append(rows, VolumeRow{
			Volume:      name,
			Status:      asInt(cols[colVolStatus][idx], true),
			FreeBytes:   asInt64(cols[colVolFree][idx], true),
			TotalBytes:  asInt64(cols[colVolTotal][idx], true),
			HotspareCnt: asInt(cols[colVolHotspare][idx], true),
		})
	}
	return rows, nil
}

// collectSystem returns the single wide system row. Missing OIDs are
// omitted rather than zero-filled: a schema store tolerates a record that
// omits a declared field, and omitting is honest where zero would read as
// a real measurement (0°C, 0% battery).
func collectSystem(s *gosnmp.GoSNMP) (map[string]any, error) {
	oids := make([]string, 0, len(systemScalars))
	for _, sc := range systemScalars {
		oids = append(oids, sc.oid)
	}

	byOID := map[string]gosnmp.SnmpPDU{}
	// gosnmp caps a single Get at MaxOids (default 60); chunk defensively.
	for start := 0; start < len(oids); start += 30 {
		end := start + 30
		if end > len(oids) {
			end = len(oids)
		}
		res, err := s.Get(oids[start:end])
		if err != nil {
			return nil, fmt.Errorf("get system scalars: %w", err)
		}
		for _, pdu := range res.Variables {
			byOID[strings.TrimPrefix(pdu.Name, ".")] = pdu
		}
	}

	row := map[string]any{}
	for _, sc := range systemScalars {
		pdu, ok := byOID[strings.TrimPrefix(sc.oid, ".")]
		if !ok || pdu.Value == nil ||
			pdu.Type == gosnmp.NoSuchObject || pdu.Type == gosnmp.NoSuchInstance {
			continue
		}
		switch sc.kind {
		case "string":
			row[sc.field] = asString(pdu, true)
		case "float":
			// The UPS floats arrive as ASN.1 Opaque-wrapped values, which
			// gosnmp decodes to float32 (OpaqueFloat) or float64
			// (OpaqueDouble) — NOT to an integer type. Assert both before
			// falling back, or a live 100% battery reads as 0.
			switch f := pdu.Value.(type) {
			case float32:
				row[sc.field] = float64(f)
			case float64:
				row[sc.field] = f
			default:
				row[sc.field] = float64(asInt(pdu, true))
			}
		case "load":
			// laLoadInt is the load average multiplied by 100.
			row[sc.field] = float64(asInt(pdu, true)) / 100.0
		default:
			row[sc.field] = asInt(pdu, true)
		}
	}
	return row, nil
}

// ---------------------------------------------------------------------------
// ts-store writers. Same contract as docker-stats: one authenticated socket
// connection per batch, or one POST per record over HTTP.
// ---------------------------------------------------------------------------

type writer interface {
	writeBatch(store string, records [][]byte) error
}

type socketWriter struct {
	socketPath string
	keys       map[string]string // store -> API key
}

func (w *socketWriter) writeBatch(store string, records [][]byte) error {
	if len(records) == 0 {
		return nil
	}
	conn, err := net.Dial("unix", w.socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "AUTH %s %s\n", store, w.keys[store]); err != nil {
		return err
	}
	resp, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("auth failed for %s: %s", store, strings.TrimSpace(resp))
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

type httpWriter struct {
	baseURL string
	keys    map[string]string
	client  *http.Client
}

func (w *httpWriter) writeBatch(store string, records [][]byte) error {
	url := fmt.Sprintf("%s/api/stores/%s/data", w.baseURL, store)
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
		req.Header.Set("X-API-Key", w.keys[store])
		resp, err := w.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("store %s: unexpected status %d", store, resp.StatusCode)
		}
	}
	return nil
}

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

// ---------------------------------------------------------------------------
// Tick.
// ---------------------------------------------------------------------------

// runTick performs one full poll. A failure in any one stream is logged and
// the others still run: a NAS that reports disks but has no UPS attached
// should not lose disk history to a UPS error.
func runTick(s *gosnmp.GoSNMP, w writer, stores map[string]string, toStdout bool) {
	if err := s.Connect(); err != nil {
		log.Printf("snmp connect: %v", err)
		return
	}
	defer s.Conn.Close()

	if disks, err := collectDisks(s); err != nil {
		log.Printf("disks: %v", err)
	} else if recs, err := marshalAll(disks); err != nil {
		log.Printf("disks marshal: %v", err)
	} else if toStdout {
		printRecords(recs)
	} else if err := w.writeBatch(stores["disks"], recs); err != nil {
		log.Printf("disks write: %v", err)
	}

	if vols, err := collectVolumes(s); err != nil {
		log.Printf("volumes: %v", err)
	} else if recs, err := marshalAll(vols); err != nil {
		log.Printf("volumes marshal: %v", err)
	} else if toStdout {
		printRecords(recs)
	} else if err := w.writeBatch(stores["volumes"], recs); err != nil {
		log.Printf("volumes write: %v", err)
	}

	if sys, err := collectSystem(s); err != nil {
		log.Printf("system: %v", err)
	} else if b, err := json.Marshal(sys); err != nil {
		log.Printf("system marshal: %v", err)
	} else if toStdout {
		printRecords([][]byte{b})
	} else if err := w.writeBatch(stores["system"], [][]byte{b}); err != nil {
		log.Printf("system write: %v", err)
	}
}

func printRecords(recs [][]byte) {
	for _, r := range recs {
		fmt.Println(string(r))
	}
}

func main() {
	var (
		target      = flag.String("target", "", "Synology hostname or IP (required)")
		port        = flag.Int("port", 161, "SNMP UDP port")
		user        = flag.String("user", "", "SNMPv3 username (or SNMP_USER)")
		authPass    = flag.String("auth-pass", "", "SNMPv3 auth passphrase (or SNMP_AUTH_PASS)")
		privPass    = flag.String("priv-pass", "", "SNMPv3 privacy passphrase (or SNMP_PRIV_PASS)")
		socketPath  = flag.String("socket", "", "ts-store Unix socket path")
		httpURL     = flag.String("http", "", "ts-store HTTP URL (use instead of socket)")
		storePrefix = flag.String("store-prefix", "synology", "Store name prefix: <prefix>-disks/-volumes/-system")
		disksKey    = flag.String("disks-key", "", "API key for the disks store (or TSSTORE_DISKS_KEY)")
		volumesKey  = flag.String("volumes-key", "", "API key for the volumes store (or TSSTORE_VOLUMES_KEY)")
		systemKey   = flag.String("system-key", "", "API key for the system store (or TSSTORE_SYSTEM_KEY)")
		interval    = flag.Int("interval", 60, "Poll interval in seconds")
		timeout     = flag.Int("timeout", 10, "SNMP timeout in seconds")
		stdout      = flag.Bool("stdout", false, "Print to stdout instead of writing to ts-store")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("synology-snmp", Version)
		return
	}

	// Secrets come from the environment by default so they never appear in
	// argv / `ps` output on a shared host.
	if *user == "" {
		*user = os.Getenv("SNMP_USER")
	}
	if *authPass == "" {
		*authPass = os.Getenv("SNMP_AUTH_PASS")
	}
	if *privPass == "" {
		*privPass = os.Getenv("SNMP_PRIV_PASS")
	}
	if *httpURL == "" {
		*httpURL = os.Getenv("TSSTORE_URL")
	}
	if *disksKey == "" {
		*disksKey = os.Getenv("TSSTORE_DISKS_KEY")
	}
	if *volumesKey == "" {
		*volumesKey = os.Getenv("TSSTORE_VOLUMES_KEY")
	}
	if *systemKey == "" {
		*systemKey = os.Getenv("TSSTORE_SYSTEM_KEY")
	}

	if *target == "" || *user == "" || *authPass == "" || *privPass == "" {
		log.Fatal("-target, -user, -auth-pass and -priv-pass are all required " +
			"(passphrases may come from SNMP_AUTH_PASS / SNMP_PRIV_PASS)")
	}
	if *interval < 1 {
		log.Fatal("-interval must be >= 1 second")
	}

	stores := map[string]string{
		"disks":   *storePrefix + "-disks",
		"volumes": *storePrefix + "-volumes",
		"system":  *storePrefix + "-system",
	}

	var w writer
	if !*stdout {
		keys := map[string]string{
			stores["disks"]:   *disksKey,
			stores["volumes"]: *volumesKey,
			stores["system"]:  *systemKey,
		}
		switch {
		case *httpURL != "":
			w = &httpWriter{baseURL: strings.TrimRight(*httpURL, "/"), keys: keys,
				client: &http.Client{Timeout: 30 * time.Second}}
		case *socketPath != "":
			w = &socketWriter{socketPath: *socketPath, keys: keys}
		default:
			log.Fatal("one of -http or -socket is required (or use -stdout)")
		}
	}

	s := newSNMP(*target, uint16(*port), *user, *authPass, *privPass,
		time.Duration(*timeout)*time.Second)

	log.Printf("synology-snmp %s polling %s every %ds -> %s-{disks,volumes,system}",
		Version, *target, *interval, *storePrefix)

	runTick(s, w, stores, *stdout)
	ticker := time.NewTicker(time.Duration(*interval) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		runTick(s, w, stores, *stdout)
	}
}
