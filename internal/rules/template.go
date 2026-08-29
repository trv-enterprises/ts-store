// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Message templates (issue #144).
//
// An alert payload carries rule_name, condition and a data blob, none of
// which is a sentence. Every receiver — Slack, a notification service,
// the dashboard's bell row — otherwise assembles its own human-readable
// text from those parts, so the same formatting gets reimplemented per
// consumer and drifts. A per-rule template fixes it once, where someone
// already knows what the rule means.
//
//	"Server Room 5's temperature {temp} exceeded the max"
//	  -> "Server Room 5's temperature 95 exceeded the max"
//
// Rendering is deliberately total: it returns a string and no error.
// A malformed placeholder renders empty rather than failing, because a
// formatting mistake must never suppress the alert it was describing —
// the alert is the thing that matters, the prose is decoration.

// MaxMessageTemplateBytes caps the size of a message template's SOURCE
// (not its rendered output), mirroring MaxExternalRefBytes so a rogue
// config cannot bloat every payload this rule ever emits. The source is
// what we can bound cheaply at write time; the rendered length varies
// per fire with the data.
const MaxMessageTemplateBytes = 512

// maxPrecision bounds the N in {field:.Nf}. Ten decimal places is past
// the precision float64 carries anyway, and an unbounded N is a way to
// inflate every rendered message.
const maxPrecision = 10

// reservedVars are the built-in template variables. A record field with
// one of these names is shadowed by the built-in — narrow risk, and
// documented. If it ever bites, a {data.<field>} escape hatch can be
// added without changing existing templates.
var reservedVars = []string{"store", "rule_name", "condition", "timestamp", "external_ref"}

// Epoch magnitude boundaries for {field:time}. ts-store stores
// nanoseconds, but a user's own field may hold seconds or milliseconds,
// and the unit is not recoverable from the type. Rather than guess per
// call site, the unit is inferred from magnitude.
//
// The tradeoff, stated plainly: a genuine NANOSECOND value below 1e11
// (i.e. an instant in the first ~100 seconds after the epoch) is read as
// seconds and renders a 1970s-or-later date instead of 1970-01-01. That
// requires a timestamp from the first minute and a half of 1970, which
// ts-store never produces and no plausible sensor field carries. The
// boundaries are pinned by tests so they are explicit, not incidental.
const (
	// 1e11 seconds is year 5138; 1e11 ms is 1973. Anything smaller is
	// far likelier to be seconds than milliseconds.
	thresholdSeconds = int64(1e11)
	// 1e14 ms is year 5138; 1e14 µs is 1973.
	thresholdMillis = int64(1e14)
	// 1e17 µs is year 5138; 1e17 ns is 1973.
	thresholdMicros = int64(1e17)
)

// ValidateMessageTemplate checks a template's source at write time.
//
// It deliberately does NOT verify that referenced fields exist: for JSON
// stores the field set is not known until data arrives, so create-time
// field validation would either reject valid templates or give false
// confidence. Unknown fields are a render-time concern, and render empty.
func ValidateMessageTemplate(tmpl string) error {
	if len(tmpl) > MaxMessageTemplateBytes {
		return fmt.Errorf("message template too long: %d bytes (max %d)",
			len(tmpl), MaxMessageTemplateBytes)
	}
	if strings.IndexByte(tmpl, 0) >= 0 {
		return fmt.Errorf("message template contains a null byte")
	}
	return nil
}

// RenderMessage renders a template against an alert's variables.
//
// Placeholders are {name} or {name:spec}. `{{` and `}}` are literal
// braces. An unclosed `{` is emitted literally, so a message that merely
// mentions a brace does not silently truncate.
//
// Never returns an error: see the package comment above.
func RenderMessage(tmpl string, vars map[string]interface{}) string {
	if tmpl == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(tmpl))

	for i := 0; i < len(tmpl); {
		c := tmpl[i]

		if c == '{' {
			// `{{` is a literal brace.
			if i+1 < len(tmpl) && tmpl[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				// Unclosed brace: emit the rest literally rather than
				// swallowing it. A truncated message hides more than a
				// visibly odd one.
				b.WriteString(tmpl[i:])
				break
			}
			name, spec := splitPlaceholder(tmpl[i+1 : i+end])
			b.WriteString(renderVar(name, spec, vars))
			i += end + 1
			continue
		}

		// `}}` is a literal brace; a lone `}` is already literal.
		if c == '}' && i+1 < len(tmpl) && tmpl[i+1] == '}' {
			b.WriteByte('}')
			i += 2
			continue
		}

		b.WriteByte(c)
		i++
	}

	return b.String()
}

// TemplateVars builds the variable set for one fire: every field of the
// triggering record, with the built-ins layered on top.
//
// Built-ins win on collision. Field names are referenced bare ({temp},
// not {data.temp}), so a record field named "store" or "timestamp" is
// shadowed. That is a narrow risk on a documented, fixed list, and the
// alternative — record fields shadowing {store} — would mean a template
// silently changing meaning when a new field appears in the data.
func TemplateVars(data map[string]interface{}, storeName, ruleName, condition, externalRef string, timestamp int64) map[string]interface{} {
	vars := make(map[string]interface{}, len(data)+len(reservedVars))
	for k, v := range data {
		vars[k] = v
	}
	vars["store"] = storeName
	vars["rule_name"] = ruleName
	vars["condition"] = condition
	vars["timestamp"] = timestamp
	vars["external_ref"] = externalRef
	return vars
}

// splitPlaceholder splits "field:spec" into its parts. Field names are
// [\w.-]+ (the same charset the condition parser accepts), so a colon is
// unambiguous as the separator.
func splitPlaceholder(body string) (name, spec string) {
	if idx := strings.IndexByte(body, ':'); idx >= 0 {
		return strings.TrimSpace(body[:idx]), strings.TrimSpace(body[idx+1:])
	}
	return strings.TrimSpace(body), ""
}

// renderVar resolves one placeholder. A missing field, a nil value, and
// a field whose name was misspelled all render empty — indistinguishable
// by design, since none of them should cost the operator an alert.
func renderVar(name, spec string, vars map[string]interface{}) string {
	if name == "" {
		return ""
	}
	v, ok := vars[name]
	if !ok || v == nil {
		return ""
	}
	return formatTemplateValue(v, spec)
}

// formatTemplateValue renders a single value, honoring an optional spec.
//
// Specs apply only where they are meaningful: `.Nf` to numbers, `time`
// to integers. Applied to anything else the spec is IGNORED and the
// value renders normally, rather than producing an error string. A
// receiver reading "temperature nas-01" can tell something is off; a
// receiver reading "temperature %!d(string=nas-01)" learns nothing and
// the operator gets a corrupted page.
func formatTemplateValue(v interface{}, spec string) string {
	switch spec {
	case "":
		// no spec — fall through to default rendering
	case "time":
		if ts, ok := asInt64(v); ok {
			return formatEpoch(ts)
		}
	default:
		if prec, ok := parsePrecisionSpec(spec); ok {
			if f, ok := asFloat64(v); ok {
				return strconv.FormatFloat(f, 'f', prec, 64)
			}
		}
	}

	return defaultFormat(v)
}

// defaultFormat renders a value with no spec.
//
// Floats use 'f' with -1 precision rather than %v: %v switches to
// scientific notation past ~1e21 and for very small magnitudes, so a
// field holding a large integer-valued float would render as "1.747e+18"
// in the middle of a sentence. 'f' keeps it readable and still drops a
// trailing ".0", so 95.0 renders "95" and 95.5 renders "95.5".
func defaultFormat(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case float32:
		// Format at float32 precision: widening 0.1 to float64 yields
		// 0.10000000149011612, which is an artifact of the conversion
		// rather than anything the user stored.
		return strconv.FormatFloat(float64(val), 'f', -1, 32)
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			// NaN/±Inf have no useful reading in prose, and "NaN" in an
			// alert is noise. Render empty, consistent with a missing value.
			return ""
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", val)
	case time.Time:
		return val.UTC().Format(time.RFC3339)
	case map[string]interface{}, []interface{}:
		// Compact JSON: lossless, and a receiver forwarding the string
		// still sees real content. Braces in the output are literal —
		// rendering is single-pass, so they are never re-scanned as
		// placeholders.
		if b, err := json.Marshal(val); err == nil {
			return string(b)
		}
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

// parsePrecisionSpec parses ".Nf" (e.g. ".2f"). Returns false for
// anything else, which causes the spec to be ignored.
func parsePrecisionSpec(spec string) (int, bool) {
	if len(spec) < 3 || spec[0] != '.' || spec[len(spec)-1] != 'f' {
		return 0, false
	}
	n, err := strconv.Atoi(spec[1 : len(spec)-1])
	if err != nil || n < 0 || n > maxPrecision {
		return 0, false
	}
	return n, true
}

// formatEpoch renders an epoch integer as RFC3339 in UTC, inferring the
// unit from magnitude (see the threshold constants).
//
// UTC rather than server-local: a container without TZ set already runs
// UTC, so "local" is frequently UTC wearing a disguise, and the same
// instant would otherwise stamp differently across the fleet — painful
// exactly when correlating one incident across hosts.
func formatEpoch(ts int64) string {
	abs := ts
	if abs < 0 {
		abs = -abs
	}

	var t time.Time
	switch {
	case abs < thresholdSeconds:
		t = time.Unix(ts, 0)
	case abs < thresholdMillis:
		t = time.UnixMilli(ts)
	case abs < thresholdMicros:
		t = time.UnixMicro(ts)
	default:
		t = time.Unix(0, ts)
	}
	return t.UTC().Format(time.RFC3339)
}

// asInt64 coerces the integer-ish types that can reach a template.
// JSON stores decode every number to float64, so an epoch read from a
// JSON record arrives as a float and must be accepted here.
func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case float32:
		return int64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// asFloat64 coerces any numeric type for precision formatting.
func asFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
