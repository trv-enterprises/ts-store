// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package main

import (
	"encoding/json"
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
  test    <store> --condition <expr> --data <json>  Dry-run a condition against a sample record
  rm      <store> <alert-id>                     Delete an alert

Required rule options (every create needs these):
  --name <label>           Human label for this alert
  --condition <expr>       Rule expression, e.g. "temperature > 80"
                           (condition rules only)

Staleness rules — fire when a store STOPS receiving data:
  --rule-type staleness    Alert on absent data instead of bad values
  --max-age <duration>     Fire when nothing has arrived for this long
                           (required for staleness; no default). Mutually
                           exclusive with --condition.

Common create options:
  --cooldown <duration>    Min time between alerts (e.g., 5m)
  --external-ref <s>       Opaque tag echoed on every alert payload
  --message <template>     Human-readable sentence rendered onto each
                           alert's "message" field. {field} inserts a
                           value from the triggering record; built-ins
                           are {store}, {rule_name}, {condition},
                           {timestamp}, {external_ref}. Formats:
                           {temp:.1f} sets decimal places, {ts:time}
                           renders an epoch as RFC3339 UTC. Use {{ for
                           a literal brace. An unknown field renders
                           empty and never suppresses the alert.
  --restart now|resume     Restart policy (default now). "resume" reads
                           the cursor on Start and replays records since.
  --max-replay <duration>  When --restart=resume, cap how far back to
                           replay (e.g., 1h). Default: unbounded.
  --header K:V             Add custom header (repeatable, webhook/ws only)
  --poll <duration>        Poll interval (default 1s)
  --api-key <key>          API key (or set TSSTORE_API_KEY)

Examples:
  tsstore alerts webhook add my-store \
    --url https://hooks.slack.com/services/... \
    --name high-temp --condition "temperature > 80" --cooldown 5m

  tsstore alerts mqtt add my-store \
    --broker tcp://mqtt:1883 --topic alerts/heat \
    --name high-temp --condition "temperature > 80"

  # Fire if the collector writing this store goes quiet for 5 minutes
  tsstore alerts webhook add my-store \
    --url https://hooks.slack.com/services/... \
    --name "collector went quiet" \
    --rule-type staleness --max-age 5m --cooldown 30m

  # Send a ready-made sentence rather than making the receiver build one
  tsstore alerts webhook add my-store \
    --url https://hooks.slack.com/services/... \
    --name high-temp --condition "temperature > 80" \
    --message "Server Room 5 is at {temperature:.1f}C (limit 80)"

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
	case "test":
		runAlertsTest(args[1:])
	case "-h", "--help":
		printAlertsUsage()
	default:
		fmt.Printf("Unknown alerts subcommand: %s\n", subcommand)
		printAlertsUsage()
		os.Exit(1)
	}
}

// commonAlertFlags holds the rule + dispatch fields shared by every alert
// create command.
type commonAlertFlags struct {
	storeName     string
	apiKey        string
	name          string
	ruleType      string
	condition     string
	maxAge        string
	cooldown      string
	externalRef   string
	message       string
	restartPolicy string
	maxReplay     string
	headers       []string
	pollEvery     string
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

// applyCommonRuleFields copies the rule + dispatch fields onto the
// request body map. Used by all three transport-specific runners.
func applyCommonRuleFields(body map[string]interface{}, c commonAlertFlags) {
	body["name"] = c.name
	// Omit both when empty so the server applies its own defaults and
	// reports its own validation errors — the CLI does not second-guess
	// which rule fields are required for which rule type.
	if c.ruleType != "" {
		body["rule_type"] = c.ruleType
	}
	if c.condition != "" {
		body["condition"] = c.condition
	}
	if c.maxAge != "" {
		body["max_age"] = c.maxAge
	}
	if c.cooldown != "" {
		body["cooldown"] = c.cooldown
	}
	if c.message != "" {
		body["message"] = c.message
	}
	if c.externalRef != "" {
		body["external_ref"] = c.externalRef
	}
	if c.restartPolicy != "" {
		body["restart_policy"] = c.restartPolicy
	}
	if c.maxReplay != "" {
		body["max_replay"] = c.maxReplay
	}
	if c.pollEvery != "" {
		body["poll_interval"] = c.pollEvery
	}
}

// missingRuleFlag returns the flag the user still has to supply for the
// chosen rule type, or "" when the rule is fully specified. A staleness
// alert takes --max-age and no condition; a condition alert the reverse.
// Conflicting combinations are left to the server so the CLI and the API
// can't drift apart on which pairings are legal.
func missingRuleFlag(c commonAlertFlags) string {
	if c.ruleType == "staleness" {
		if c.maxAge == "" {
			return "--max-age"
		}
		return ""
	}
	if c.condition == "" {
		return "--condition"
	}
	return ""
}

// addCommonRuleFlags is the per-transport flag parser snippet for the
// shared rule fields. Returns the (possibly advanced) index and whether
// the flag was recognized.
func addCommonRuleFlag(args []string, i int, c *commonAlertFlags, flag string) (int, bool) {
	switch flag {
	case "--name":
		return consumeFlag(args, i, &c.name)
	case "--rule-type":
		return consumeFlag(args, i, &c.ruleType)
	case "--condition":
		return consumeFlag(args, i, &c.condition)
	case "--max-age":
		return consumeFlag(args, i, &c.maxAge)
	case "--cooldown":
		return consumeFlag(args, i, &c.cooldown)
	case "--external-ref":
		return consumeFlag(args, i, &c.externalRef)
	case "--message":
		return consumeFlag(args, i, &c.message)
	case "--restart":
		return consumeFlag(args, i, &c.restartPolicy)
	case "--max-replay":
		return consumeFlag(args, i, &c.maxReplay)
	case "--poll":
		return consumeFlag(args, i, &c.pollEvery)
	case "--api-key":
		return consumeFlag(args, i, &c.apiKey)
	}
	return i, false
}

// runAlertsWebhook implements `tsstore alerts webhook add <store> [options]`.
func runAlertsWebhook(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts webhook add <store> --url <url> --name <n> --condition <c> [options]")
		os.Exit(1)
	}
	args = args[1:]

	c := commonAlertFlags{}
	var url, timeout string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts webhook add <store> --url <url> --name <n> --condition <c> [--cooldown <d>] [--external-ref <s>] [--message <tmpl>] [--restart now|resume] [--max-replay <d>] [--header K:V] [--poll <d>] [--timeout <d>] [--api-key <k>]")
			return
		case "--url":
			i, _ = consumeFlag(args, i, &url)
		case "--api-key", "--name", "--rule-type", "--condition", "--max-age", "--cooldown", "--external-ref", "--message", "--restart", "--max-replay", "--poll":
			i, _ = addCommonRuleFlag(args, i, &c, args[i])
		case "--header":
			i, _ = consumeAppend(args, i, &c.headers)
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

	if c.storeName == "" || url == "" || c.name == "" {
		fmt.Println("Error: store name, --url, and --name are required")
		os.Exit(1)
	}
	if missing := missingRuleFlag(c); missing != "" {
		fmt.Printf("Error: %s is required\n", missing)
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

	webhook := map[string]interface{}{"url": url}
	if headers != nil {
		webhook["headers"] = headers
	}
	if timeout != "" {
		webhook["timeout"] = timeout
	}
	body := map[string]interface{}{
		"type":    "webhook",
		"webhook": webhook,
	}
	applyCommonRuleFields(body, c)

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts", c.storeName), body)
}

// runAlertsWS implements `tsstore alerts ws add <store> [options]`.
func runAlertsWS(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts ws add <store> --url <ws-url> --name <n> --condition <c> [options]")
		os.Exit(1)
	}
	args = args[1:]

	c := commonAlertFlags{}
	var url string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts ws add <store> --url <ws-url> --name <n> --condition <c> [--cooldown <d>] [--external-ref <s>] [--message <tmpl>] [--restart now|resume] [--max-replay <d>] [--header K:V] [--poll <d>] [--api-key <k>]")
			return
		case "--url":
			i, _ = consumeFlag(args, i, &url)
		case "--api-key", "--name", "--rule-type", "--condition", "--max-age", "--cooldown", "--external-ref", "--message", "--restart", "--max-replay", "--poll":
			i, _ = addCommonRuleFlag(args, i, &c, args[i])
		case "--header":
			i, _ = consumeAppend(args, i, &c.headers)
		default:
			if c.storeName == "" && !strings.HasPrefix(args[i], "-") {
				c.storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if c.storeName == "" || url == "" || c.name == "" {
		fmt.Println("Error: store name, --url, and --name are required")
		os.Exit(1)
	}
	if missing := missingRuleFlag(c); missing != "" {
		fmt.Printf("Error: %s is required\n", missing)
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

	ws := map[string]interface{}{"url": url}
	if headers != nil {
		ws["headers"] = headers
	}
	body := map[string]interface{}{
		"type": "ws",
		"ws":   ws,
	}
	applyCommonRuleFields(body, c)

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts", c.storeName), body)
}

// runAlertsMQTT implements `tsstore alerts mqtt add <store> [options]`.
func runAlertsMQTT(args []string) {
	if len(args) < 1 || args[0] != "add" {
		fmt.Println("Usage: tsstore alerts mqtt add <store> --broker <url> --topic <t> --name <n> --condition <c> [options]")
		os.Exit(1)
	}
	args = args[1:]

	c := commonAlertFlags{}
	var broker, topic, username, password, qos string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Println("Usage: tsstore alerts mqtt add <store> --broker <url> --topic <t> --name <n> --condition <c> [--cooldown <d>] [--external-ref <s>] [--message <tmpl>] [--restart now|resume] [--max-replay <d>] [--qos 0|1|2] [--username U --password P] [--poll <d>] [--api-key <k>]")
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
		case "--api-key", "--name", "--rule-type", "--condition", "--max-age", "--cooldown", "--external-ref", "--message", "--restart", "--max-replay", "--poll":
			i, _ = addCommonRuleFlag(args, i, &c, args[i])
		default:
			if c.storeName == "" && !strings.HasPrefix(args[i], "-") {
				c.storeName = args[i]
			} else {
				fmt.Printf("Unknown option: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if c.storeName == "" || broker == "" || topic == "" || c.name == "" {
		fmt.Println("Error: store name, --broker, --topic, and --name are required")
		os.Exit(1)
	}
	if missing := missingRuleFlag(c); missing != "" {
		fmt.Printf("Error: %s is required\n", missing)
		os.Exit(1)
	}

	apiKey := resolveAPIKey(c.apiKey)
	if apiKey == "" {
		fmt.Println("Error: API key required (--api-key or TSSTORE_API_KEY)")
		os.Exit(1)
	}

	mqtt := map[string]interface{}{
		"broker_url": broker,
		"topic":      topic,
	}
	if username != "" {
		mqtt["username"] = username
	}
	if password != "" {
		mqtt["password"] = password
	}
	if qos != "" {
		var q int
		fmt.Sscanf(qos, "%d", &q)
		mqtt["qos"] = q
	}
	body := map[string]interface{}{
		"type": "mqtt",
		"mqtt": mqtt,
	}
	applyCommonRuleFields(body, c)

	cfg := loadStreamConfig()
	apiPost(cfg, apiKey, fmt.Sprintf("/api/stores/%s/alerts", c.storeName), body)
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

// runAlertsTest dry-runs a condition against a sample record via
// POST /alerts/test — no alert is created.
func runAlertsTest(args []string) {
	var storeName, apiKey, condition, dataJSON string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--condition":
			i, _ = consumeFlag(args, i, &condition)
		case "--data":
			i, _ = consumeFlag(args, i, &dataJSON)
		case "--api-key":
			i, _ = consumeFlag(args, i, &apiKey)
		case "-h", "--help":
			fmt.Println(`Usage: tsstore alerts test <store> --condition <expr> --data <json> [--api-key <k>]

Dry-runs a rule condition against a sample record without creating an
alert. Shows how the condition parsed and whether it would have fired.

Example:
  tsstore alerts test sensors --condition "temperature > 80" --data '{"temperature": 95}'`)
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
	if storeName == "" || condition == "" {
		fmt.Println("Error: store name and --condition are required")
		os.Exit(1)
	}
	var data map[string]interface{}
	if dataJSON != "" {
		if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
			fmt.Printf("Error: --data is not valid JSON: %v\n", err)
			os.Exit(1)
		}
	}
	key := resolveAPIKey(apiKey)
	if key == "" {
		fmt.Println("Error: API key required")
		os.Exit(1)
	}
	cfg := loadStreamConfig()
	body := map[string]interface{}{"condition": condition, "data": data}
	apiPost(cfg, key, fmt.Sprintf("/api/stores/%s/alerts/test", storeName), body)
}
