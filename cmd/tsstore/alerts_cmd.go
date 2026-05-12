// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package main

import (
	"fmt"
	"os"
	"strings"
)

func printAlertsUsage() {
	fmt.Println(`tsstore alerts - Manage alert resources

Usage:
  tsstore alerts <subcommand> [arguments]

Subcommands:
  webhook add <store> --url <url>     [options]  Create a webhook alert
  ws      add <store> --url <ws-url>  [options]  Create a WS alert
  mqtt    add <store> --broker <url> --topic <t> [options]  Create an MQTT alert
  list    <store>                                List all alerts for a store
  rm      <store> <alert-id>                     Delete an alert

Common create options:
  --rule "name:condition"  Add a rule (repeatable). Example:
                           --rule "high-temp:temperature > 80"
  --cooldown <duration>    Apply cooldown to the last --rule (e.g., 5m)
  --header K=V             Add custom header (repeatable, webhook/ws only)
  --poll <duration>        Poll interval (default 1s)
  --api-key <key>          API key (or set TSSTORE_API_KEY)

Examples:
  tsstore alerts webhook add my-store \
    --url https://hooks.slack.com/services/... \
    --rule "high-temp:temperature > 80" --cooldown 5m

  tsstore alerts mqtt add my-store \
    --broker tcp://mqtt:1883 --topic alerts/heat \
    --rule "high-temp:temperature > 80"

  tsstore alerts list my-store
  tsstore alerts rm   my-store a1b2c3d4`)
}

func runAlertsCommand(args []string) {
	subcommand := args[0]
	switch subcommand {
	case "webhook":
		runAlertsWebhook(args[1:])
	case "ws":
		runAlertsWS(args[1:])
	case "mqtt":
		runAlertsMQTT(args[1:])
	case "list":
		runAlertsList(args[1:])
	case "rm", "delete", "remove":
		runAlertsRm(args[1:])
	case "-h", "--help":
		printAlertsUsage()
	default:
		fmt.Printf("Unknown alerts subcommand: %s\n", subcommand)
		printAlertsUsage()
		os.Exit(1)
	}
}

// commonAlertFlags holds fields shared across webhook/ws/mqtt alert create.
type commonAlertFlags struct {
	storeName string
	apiKey    string
	rules     []map[string]string // ordered: each rule has name, condition, cooldown
	headers   []string
	pollEvery string
}

// parseRuleFlag splits "name:condition" into a rule entry. Condition may
// contain colons; only the FIRST colon separates name from condition.
func parseRuleFlag(s string) (map[string]string, error) {
	name, cond, ok := strings.Cut(s, ":")
	if !ok || name == "" || cond == "" {
		return nil, fmt.Errorf("invalid --rule %q (expected name:condition)", s)
	}
	return map[string]string{"name": strings.TrimSpace(name), "condition": strings.TrimSpace(cond)}, nil
}

// rulesAsJSONList converts parsed rules into the API's []AlertRuleConfig shape.
func rulesAsJSONList(rules []map[string]string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		entry := map[string]interface{}{
			"name":      r["name"],
			"condition": r["condition"],
		}
		if cd := r["cooldown"]; cd != "" {
			entry["cooldown"] = cd
		}
		out = append(out, entry)
	}
	return out
}

// parseHeaders turns ["K1:V1", "K2:V2"] into a map.
func parseHeaders(hdrs []string) (map[string]string, error) {
	if len(hdrs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(hdrs))
	for _, h := range hdrs {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("invalid header format %q (expected key:value)", h)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// parseCommonAlertFlags walks args once, extracting shared flags (rules,
// cooldowns, headers, store name, api key, poll). The transport-specific
// runner is expected to consume its own flags first via consumeFlag and
// pass the remaining slice into this helper.
//
// Returns the parsed common flags and a slice of unrecognized args so the
// caller can complain about them.
func consumeFlag(args []string, i int, dst *string) (int, bool) {
	if i+1 >= len(args) {
		return i, false
	}
	*dst = args[i+1]
	return i + 1, true
}

func consumeAppend(args []string, i int, dst *[]string) (int, bool) {
	if i+1 >= len(args) {
		return i, false
	}
	*dst = append(*dst, args[i+1])
	return i + 1, true
}

// runAlertsWebhook implements `tsstore alerts webhook add <store> [options]`.
func runAlertsWebhook(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts webhook add <store> --url <url> --rule \"name:condition\" [options]")
		os.Exit(1)
	}
	args = args[1:] // drop "add"

	c := commonAlertFlags{}
	var url, timeout string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts webhook add <store> --url <url> --rule \"name:condition\" [--cooldown <d>] [--header K:V] [--poll <d>] [--timeout <d>] [--api-key <k>]")
			return
		case "--url":
			i, _ = consumeFlag(args, i, &url)
		case "--api-key":
			i, _ = consumeFlag(args, i, &c.apiKey)
		case "--rule":
			if i+1 >= len(args) {
				fmt.Println("Error: --rule requires a value")
				os.Exit(1)
			}
			rule, err := parseRuleFlag(args[i+1])
			if err != nil {
				fmt.Println("Error:", err)
				os.Exit(1)
			}
			c.rules = append(c.rules, rule)
			i++
		case "--cooldown":
			if len(c.rules) == 0 {
				fmt.Println("Error: --cooldown must follow a --rule")
				os.Exit(1)
			}
			i, _ = consumeFlag(args, i, new(string))
			c.rules[len(c.rules)-1]["cooldown"] = args[i]
		case "--header":
			i, _ = consumeAppend(args, i, &c.headers)
		case "--poll":
			i, _ = consumeFlag(args, i, &c.pollEvery)
		case "--timeout":
			i, _ = consumeFlag(args, i, &timeout)
		default:
			if c.storeName == "" && !strings.HasPrefix(args[i], "-") {
				c.storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if c.storeName == "" || url == "" || len(c.rules) == 0 {
		fmt.Println("Error: store name, --url, and at least one --rule are required")
		os.Exit(1)
	}

	headers, err := parseHeaders(c.headers)
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}

	apiKey := resolveAPIKey(c.apiKey)
	if apiKey == "" {
		fmt.Println("Error: API key required (--api-key or TSSTORE_API_KEY)")
		os.Exit(1)
	}

	body := map[string]interface{}{
		"url":   url,
		"rules": rulesAsJSONList(c.rules),
	}
	if headers != nil {
		body["headers"] = headers
	}
	if c.pollEvery != "" {
		body["poll_interval"] = c.pollEvery
	}
	if timeout != "" {
		body["timeout"] = timeout
	}

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts/webhook", c.storeName), body)
}

// runAlertsWS implements `tsstore alerts ws add <store> [options]`.
func runAlertsWS(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts ws add <store> --url <ws-url> --rule \"name:condition\" [options]")
		os.Exit(1)
	}
	args = args[1:]

	c := commonAlertFlags{}
	var url string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts ws add <store> --url <ws-url> --rule \"name:condition\" [--cooldown <d>] [--header K:V] [--poll <d>] [--api-key <k>]")
			return
		case "--url":
			i, _ = consumeFlag(args, i, &url)
		case "--api-key":
			i, _ = consumeFlag(args, i, &c.apiKey)
		case "--rule":
			if i+1 >= len(args) {
				fmt.Println("Error: --rule requires a value")
				os.Exit(1)
			}
			rule, err := parseRuleFlag(args[i+1])
			if err != nil {
				fmt.Println("Error:", err)
				os.Exit(1)
			}
			c.rules = append(c.rules, rule)
			i++
		case "--cooldown":
			if len(c.rules) == 0 {
				fmt.Println("Error: --cooldown must follow a --rule")
				os.Exit(1)
			}
			i, _ = consumeFlag(args, i, new(string))
			c.rules[len(c.rules)-1]["cooldown"] = args[i]
		case "--header":
			i, _ = consumeAppend(args, i, &c.headers)
		case "--poll":
			i, _ = consumeFlag(args, i, &c.pollEvery)
		default:
			if c.storeName == "" && !strings.HasPrefix(args[i], "-") {
				c.storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if c.storeName == "" || url == "" || len(c.rules) == 0 {
		fmt.Println("Error: store name, --url, and at least one --rule are required")
		os.Exit(1)
	}

	headers, err := parseHeaders(c.headers)
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}

	apiKey := resolveAPIKey(c.apiKey)
	if apiKey == "" {
		fmt.Println("Error: API key required (--api-key or TSSTORE_API_KEY)")
		os.Exit(1)
	}

	body := map[string]interface{}{
		"url":   url,
		"rules": rulesAsJSONList(c.rules),
	}
	if headers != nil {
		body["headers"] = headers
	}
	if c.pollEvery != "" {
		body["poll_interval"] = c.pollEvery
	}

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts/ws", c.storeName), body)
}

// runAlertsMQTT implements `tsstore alerts mqtt add <store> [options]`.
func runAlertsMQTT(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts mqtt add <store> --broker <url> --topic <t> --rule \"name:condition\" [options]")
		os.Exit(1)
	}
	args = args[1:]

	c := commonAlertFlags{}
	var broker, topic, username, password, qos string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts mqtt add <store> --broker <url> --topic <t> --rule \"name:condition\" [--cooldown <d>] [--qos 0|1|2] [--username U --password P] [--poll <d>] [--api-key <k>]")
			return
		case "--broker":
			i, _ = consumeFlag(args, i, &broker)
		case "--topic":
			i, _ = consumeFlag(args, i, &topic)
		case "--qos":
			i, _ = consumeFlag(args, i, &qos)
		case "--username":
			i, _ = consumeFlag(args, i, &username)
		case "--password":
			i, _ = consumeFlag(args, i, &password)
		case "--api-key":
			i, _ = consumeFlag(args, i, &c.apiKey)
		case "--rule":
			if i+1 >= len(args) {
				fmt.Println("Error: --rule requires a value")
				os.Exit(1)
			}
			rule, err := parseRuleFlag(args[i+1])
			if err != nil {
				fmt.Println("Error:", err)
				os.Exit(1)
			}
			c.rules = append(c.rules, rule)
			i++
		case "--cooldown":
			if len(c.rules) == 0 {
				fmt.Println("Error: --cooldown must follow a --rule")
				os.Exit(1)
			}
			i, _ = consumeFlag(args, i, new(string))
			c.rules[len(c.rules)-1]["cooldown"] = args[i]
		case "--poll":
			i, _ = consumeFlag(args, i, &c.pollEvery)
		default:
			if c.storeName == "" && !strings.HasPrefix(args[i], "-") {
				c.storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if c.storeName == "" || broker == "" || topic == "" || len(c.rules) == 0 {
		fmt.Println("Error: store name, --broker, --topic, and at least one --rule are required")
		os.Exit(1)
	}

	apiKey := resolveAPIKey(c.apiKey)
	if apiKey == "" {
		fmt.Println("Error: API key required (--api-key or TSSTORE_API_KEY)")
		os.Exit(1)
	}

	body := map[string]interface{}{
		"broker_url": broker,
		"topic":      topic,
		"rules":      rulesAsJSONList(c.rules),
	}
	if username != "" {
		body["username"] = username
	}
	if password != "" {
		body["password"] = password
	}
	if qos != "" {
		// Server parses as byte; we just pass through as number.
		var q int
		fmt.Sscanf(qos, "%d", &q)
		body["qos"] = q
	}
	if c.pollEvery != "" {
		body["poll_interval"] = c.pollEvery
	}

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts/mqtt", c.storeName), body)
}

func runAlertsList(args []string) {
	var storeName, apiKey string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts list <store> [--api-key <k>]")
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
	apiGet(cfg, key, fmt.Sprintf("/api/stores/%s/alerts", storeName))
}

func runAlertsRm(args []string) {
	var storeName, alertID, apiKey string
	positional := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts rm <store> <alert-id> [--api-key <k>]")
			return
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
			switch positional {
			case 0:
				storeName = args[i]
			case 1:
				alertID = args[i]
			default:
				fmt.Println("Error: too many arguments")
				os.Exit(1)
			}
			positional++
		}
	}
	if storeName == "" || alertID == "" {
		fmt.Println("Usage: tsstore alerts rm <store> <alert-id>")
		os.Exit(1)
	}
	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required")
		os.Exit(1)
	}
	cfg := loadStreamConfig()
	apiDelete(cfg, key, fmt.Sprintf("/api/stores/%s/alerts/%s", storeName, alertID))
}
