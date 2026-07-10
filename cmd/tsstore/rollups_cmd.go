// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func printRollupsUsage() {
	fmt.Println(`tsstore rollups - Manage rollup aggregations

A rollup periodically aggregates a high-frequency SOURCE store into clock-aligned
time windows and writes one record per closed window into a second TARGET store
(auto-created and sized from --retention). Subcommands are scoped to the source.

Usage:
  tsstore rollups <subcommand> [arguments]

Subcommands:
  add  <store> --window <d> [options]   Create a rollup on <store>
  list <store>                          List rollups for a store
  get  <store> <rollup-id>              Show one rollup's status
  rm   <store> <rollup-id>              Delete a rollup (target store kept
                                        unless --delete-target is passed)

Create options:
  --window <duration>      Aggregation window, e.g. 1m, 1h, 1d (required)
  --fields <spec>          Per-field aggregation, e.g. "cpu:avg+max,mem:avg"
  --default <funcs>        Functions applied to every numeric field, e.g.
                           "min,max,avg" (covers all numeric params)
  --retention <duration>   How long the target keeps rows, e.g. 1y, 90d (sizes
                           the target; default 1y)
  --target <store>         Target store name (default: <store>-<window>)
  --poll <duration>        How often the worker scans (default 30s)
  --restart resume|now     Restart policy (default resume)
  --edge-tolerance <f>     Max over-retention fraction, picks partitions (0.10)
  --force-recreate         Flush/recreate the target to apply changed params
  --api-key <key>          API key (or set TSSTORE_API_KEY)

At least one of --fields or --default is required.

Examples:
  # Hourly min/max/avg for every numeric field in system-stats:
  tsstore rollups add system-stats --window 1h --default "min,max,avg" --retention 1y

  # Per-field spec, minute windows kept 90 days:
  tsstore rollups add sensors --window 1m --fields "temp:avg+max,humidity:avg" --retention 90d

  tsstore rollups list system-stats
  tsstore rollups get  system-stats a1b2c3d4
  tsstore rollups rm   system-stats a1b2c3d4
  tsstore rollups rm   system-stats a1b2c3d4 --delete-target`)
}

func runRollupsCommand(args []string) {
	subcommand := args[0]
	switch subcommand {
	case "add", "create":
		runRollupsAdd(args[1:])
	case "list", "ls":
		runRollupsList(args[1:])
	case "get":
		runRollupsGet(args[1:])
	case "rm", "delete", "remove":
		runRollupsRm(args[1:])
	case "-h", "--help":
		printRollupsUsage()
	default:
		fmt.Printf("Unknown rollups subcommand: %s\n", subcommand)
		printRollupsUsage()
		os.Exit(1)
	}
}

func runRollupsAdd(args []string) {
	var (
		storeName, apiKey                      string
		window, fields, def, retention, target string
		poll, restart, edgeTolerance           string
		forceRecreate                          bool
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			printRollupsUsage()
			return
		case "--window":
			i, _ = consumeFlag(args, i, &window)
		case "--fields":
			i, _ = consumeFlag(args, i, &fields)
		case "--default":
			i, _ = consumeFlag(args, i, &def)
		case "--retention":
			i, _ = consumeFlag(args, i, &retention)
		case "--target":
			i, _ = consumeFlag(args, i, &target)
		case "--poll":
			i, _ = consumeFlag(args, i, &poll)
		case "--restart":
			i, _ = consumeFlag(args, i, &restart)
		case "--edge-tolerance":
			i, _ = consumeFlag(args, i, &edgeTolerance)
		case "--force-recreate":
			forceRecreate = true
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		default:
			if storeName == "" && !strings.HasPrefix(args[i], "-") {
				storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if storeName == "" || window == "" {
		fmt.Println("Error: store name and --window are required")
		os.Exit(1)
	}
	if fields == "" && def == "" {
		fmt.Println("Error: at least one of --fields or --default is required")
		os.Exit(1)
	}

	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required (--api-key or TSSTORE_API_KEY)")
		os.Exit(1)
	}

	body := map[string]interface{}{"window": window}
	if fields != "" {
		body["agg_fields"] = fields
	}
	if def != "" {
		body["agg_default"] = def
	}
	if retention != "" {
		body["retention"] = retention
	}
	if target != "" {
		body["target_store"] = target
	}
	if poll != "" {
		body["poll_interval"] = poll
	}
	if restart != "" {
		body["restart_policy"] = restart
	}
	if edgeTolerance != "" {
		if f, err := strconv.ParseFloat(edgeTolerance, 64); err == nil {
			body["edge_tolerance"] = f
		} else {
			fmt.Println("Error: --edge-tolerance must be a number")
			os.Exit(1)
		}
	}
	if forceRecreate {
		body["force_recreate"] = true
	}

	cfg := loadStreamConfig()
	apiPost(cfg, key, fmt.Sprintf("/api/stores/%s/rollups", storeName), body)
}

func runRollupsList(args []string) {
	var storeName, apiKey string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		case "-h", "--help":
			fmt.Println("Usage: tsstore rollups list <store> [--api-key <k>]")
			return
		default:
			if storeName == "" && !strings.HasPrefix(args[i], "-") {
				storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}
	if storeName == "" {
		fmt.Println("Error: store name is required")
		os.Exit(1)
	}
	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required")
		os.Exit(1)
	}
	cfg := loadStreamConfig()
	apiGet(cfg, key, fmt.Sprintf("/api/stores/%s/rollups", storeName))
}

func runRollupsGet(args []string) {
	storeName, rollupID, apiKey := parseRollupStoreAndID(args, "get")
	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required")
		os.Exit(1)
	}
	cfg := loadStreamConfig()
	apiGet(cfg, key, fmt.Sprintf("/api/stores/%s/rollups/%s", storeName, rollupID))
}

func runRollupsRm(args []string) {
	deleteTarget := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--delete-target" {
			deleteTarget = true
			continue
		}
		rest = append(rest, a)
	}
	storeName, rollupID, apiKey := parseRollupStoreAndID(rest, "rm")
	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required")
		os.Exit(1)
	}
	cfg := loadStreamConfig()
	path := fmt.Sprintf("/api/stores/%s/rollups/%s", storeName, rollupID)
	if deleteTarget {
		path += "?delete_target=true"
	}
	apiDelete(cfg, key, path)
}

// parseRollupStoreAndID parses "<store> <rollup-id> [--api-key k]" for get/rm.
func parseRollupStoreAndID(args []string, verb string) (storeName, rollupID, apiKey string) {
	positional := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		case "-h", "--help":
			fmt.Printf("Usage: tsstore rollups %s <store> <rollup-id> [--api-key <k>]\n", verb)
			os.Exit(0)
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
			switch positional {
			case 0:
				storeName = args[i]
			case 1:
				rollupID = args[i]
			default:
				fmt.Println("Error: too many arguments")
				os.Exit(1)
			}
			positional++
		}
	}
	if storeName == "" || rollupID == "" {
		fmt.Printf("Usage: tsstore rollups %s <store> <rollup-id>\n", verb)
		os.Exit(1)
	}
	return storeName, rollupID, apiKey
}
