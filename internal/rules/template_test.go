// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"strings"
	"testing"
	"time"
)

func TestRenderMessageBasics(t *testing.T) {
	vars := map[string]interface{}{
		"temp":      95.0,
		"sensor_id": "abc-1",
		"store":     "sensors",
	}

	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{"plain text", "nothing to see", "nothing to see"},
		{"empty template", "", ""},
		{"single field", "temp is {temp}", "temp is 95"},
		{"the issue's example", "Server Room 5's Temperature {temp} has exceeded the max temp",
			"Server Room 5's Temperature 95 has exceeded the max temp"},
		{"multiple fields", "{store}: {sensor_id} at {temp}", "sensors: abc-1 at 95"},
		{"repeated field", "{temp} {temp}", "95 95"},
		{"adjacent placeholders", "{temp}{temp}", "9595"},
		{"leading and trailing", "{temp} deg {temp}", "95 deg 95"},
		{"whitespace in placeholder", "{ temp }", "95"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderMessage(tt.tmpl, vars); got != tt.want {
				t.Errorf("RenderMessage(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

// A formatting mistake must never suppress the alert it was describing,
// so every malformed case renders SOMETHING rather than failing.
func TestRenderMessageNeverFails(t *testing.T) {
	vars := map[string]interface{}{"temp": 95.0, "nothing": nil}

	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{"misspelled field renders empty", "temp is {temprature}", "temp is "},
		{"missing field renders empty", "{nope}", ""},
		{"explicit nil renders empty", "[{nothing}]", "[]"},
		{"empty placeholder", "a{}b", "ab"},
		{"unclosed brace is literal", "temp {temp", "temp {temp"},
		{"unclosed at end", "value {", "value {"},
		{"lone closing brace is literal", "a } b", "a } b"},
		{"unknown spec is ignored", "{temp:bogus}", "95"},
		{"empty spec is ignored", "{temp:}", "95"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderMessage(tt.tmpl, vars); got != tt.want {
				t.Errorf("RenderMessage(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

func TestRenderMessageEscaping(t *testing.T) {
	vars := map[string]interface{}{"temp": 95.0}

	tests := []struct {
		tmpl string
		want string
	}{
		{"{{temp}}", "{temp}"},
		{"{{}}", "{}"},
		{"{{{temp}}}", "{95}"},
		{"100{{%}}", "100{%}"},
		{"{{literal}} and {temp}", "{literal} and 95"},
	}

	for _, tt := range tests {
		t.Run(tt.tmpl, func(t *testing.T) {
			if got := RenderMessage(tt.tmpl, vars); got != tt.want {
				t.Errorf("RenderMessage(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

// Default rendering must never produce scientific notation: a large
// integer-valued float in the middle of a sentence should read as a
// number, not as "1.747e+18".
func TestDefaultFormatNoScientificNotation(t *testing.T) {
	tests := []struct {
		name string
		val  interface{}
		want string
	}{
		{"integral float drops .0", 95.0, "95"},
		{"fractional float kept", 95.5, "95.5"},
		{"large float spelled out", 1.747e18, "1747000000000000000"},
		{"tiny float spelled out", 0.0000001, "0.0000001"},
		{"negative", -12.25, "-12.25"},
		{"zero", 0.0, "0"},
		{"int64", int64(42), "42"},
		{"uint8", uint8(7), "7"},
		{"int32 negative", int32(-5), "-5"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"string passthrough", "nas-01", "nas-01"},
		{"NaN renders empty", nan(), ""},
		{"Inf renders empty", inf(), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderMessage("{v}", map[string]interface{}{"v": tt.val})
			if got != tt.want {
				t.Errorf("render(%v) = %q, want %q", tt.val, got, tt.want)
			}
			if strings.ContainsAny(got, "eE") && tt.want != got {
				t.Errorf("scientific notation leaked: %q", got)
			}
		})
	}
}

// float32 formats at its own precision: widening 0.1 to float64 gives
// 0.10000000149011612, an artifact of the conversion rather than data.
func TestFloat32Precision(t *testing.T) {
	got := RenderMessage("{v}", map[string]interface{}{"v": float32(0.1)})
	if got != "0.1" {
		t.Errorf("float32(0.1) rendered %q, want %q", got, "0.1")
	}
}

func TestPrecisionSpec(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		val  interface{}
		want string
	}{
		{"one decimal", "{v:.1f}", 95.0, "95.0"},
		{"two decimals rounds", "{v:.2f}", 72.49999, "72.50"},
		{"zero decimals rounds", "{v:.0f}", 95.6, "96"},
		{"long fraction truncated", "{v:.2f}", 0.3333333333333333, "0.33"},
		{"applies to ints too", "{v:.2f}", int64(42), "42.00"},
		{"max precision allowed", "{v:.10f}", 1.5, "1.5000000000"},
		{"over max ignored", "{v:.11f}", 1.5, "1.5"},
		{"negative precision ignored", "{v:.-1f}", 1.5, "1.5"},
		{"non-numeric ignores spec", "{v:.2f}", "nas-01", "nas-01"},
		{"bool ignores spec", "{v:.2f}", true, "true"},
		{"malformed spec ignored", "{v:2f}", 1.5, "1.5"},
		{"missing f ignored", "{v:.2}", 1.5, "1.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderMessage(tt.tmpl, map[string]interface{}{"v": tt.val})
			if got != tt.want {
				t.Errorf("RenderMessage(%q, %v) = %q, want %q", tt.tmpl, tt.val, got, tt.want)
			}
		})
	}
}

// The magnitude thresholds are a deliberate heuristic. Pinning them here
// makes the boundaries explicit rather than incidental, so a future
// change to them is a visible decision.
func TestTimeSpecUnitDetection(t *testing.T) {
	// 2025-05-11T21:46:40Z in each unit.
	const (
		secs   = int64(1747000000)
		millis = int64(1747000000000)
		micros = int64(1747000000000000)
		nanos  = int64(1747000000000000000)
	)
	want := "2025-05-11T21:46:40Z"

	for _, tt := range []struct {
		name string
		val  int64
	}{
		{"seconds", secs},
		{"milliseconds", millis},
		{"microseconds", micros},
		{"nanoseconds", nanos},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderMessage("{ts:time}", map[string]interface{}{"ts": tt.val})
			if got != want {
				t.Errorf("{ts:time} on %d = %q, want %q", tt.val, got, want)
			}
		})
	}
}

func TestTimeSpecEdges(t *testing.T) {
	tests := []struct {
		name string
		val  interface{}
		want string
	}{
		{"epoch zero", int64(0), "1970-01-01T00:00:00Z"},
		{"float epoch from a JSON store", 1747000000.0, "2025-05-11T21:46:40Z"},
		{"always UTC", int64(1747000000), "2025-05-11T21:46:40Z"},
		{"string ignores spec", "not-a-time", "not-a-time"},
		{"bool ignores spec", true, "true"},
		{"time.Time renders RFC3339", time.Unix(1747000000, 0), "2025-05-11T21:46:40Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderMessage("{ts:time}", map[string]interface{}{"ts": tt.val})
			if got != tt.want {
				t.Errorf("{ts:time} on %v = %q, want %q", tt.val, got, tt.want)
			}
		})
	}
}

// The documented cost of magnitude detection: a genuine nanosecond value
// from the first ~100 seconds after the epoch reads as seconds. Asserted
// so the tradeoff is recorded in the suite, not just in a comment.
func TestTimeSpecKnownAmbiguity(t *testing.T) {
	got := RenderMessage("{ts:time}", map[string]interface{}{"ts": int64(1747000000)})
	if got == "1970-01-01T00:00:01Z" {
		t.Fatal("expected seconds interpretation for a small value")
	}
	if got != "2025-05-11T21:46:40Z" {
		t.Errorf("got %q", got)
	}
}

func TestNestedValuesRenderCompactJSON(t *testing.T) {
	vars := map[string]interface{}{
		"tags": []interface{}{"a", "b"},
		"meta": map[string]interface{}{"room": 5.0},
	}

	if got := RenderMessage("{tags}", vars); got != `["a","b"]` {
		t.Errorf("slice rendered %q", got)
	}
	if got := RenderMessage("{meta}", vars); got != `{"room":5}` {
		t.Errorf("map rendered %q", got)
	}
}

// Braces emitted by a nested value must not be re-scanned as
// placeholders — rendering is single-pass.
func TestNestedJSONBracesAreNotReparsed(t *testing.T) {
	vars := map[string]interface{}{
		"meta": map[string]interface{}{"temp": "INJECTED"},
		"temp": 95.0,
	}
	got := RenderMessage("{meta}", vars)
	if got != `{"temp":"INJECTED"}` {
		t.Errorf("got %q, want the literal JSON", got)
	}
	if strings.Contains(got, "95") {
		t.Error("nested braces were re-parsed as a placeholder")
	}
}

func TestTemplateVarsBuiltins(t *testing.T) {
	data := map[string]interface{}{"temp": 95.0}
	vars := TemplateVars(data, "sensors", "high temp", "temperature > 80", "ref-1", 1747000000000000000)

	tmpl := "{store}/{rule_name}: {condition} ({external_ref}) at {timestamp:time} temp={temp}"
	want := "sensors/high temp: temperature > 80 (ref-1) at 2025-05-11T21:46:40Z temp=95"
	if got := RenderMessage(tmpl, vars); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Built-ins shadow record fields. The alternative — data shadowing
// built-ins — would let a template silently change meaning when a new
// field shows up in the data.
func TestBuiltinsShadowRecordFields(t *testing.T) {
	data := map[string]interface{}{"store": "from-record", "temp": 1.0}
	vars := TemplateVars(data, "real-store", "r", "c", "", 0)

	if got := RenderMessage("{store}", vars); got != "real-store" {
		t.Errorf("built-in did not shadow record field: got %q", got)
	}
}

// TemplateVars must not mutate the caller's record: the same map is the
// alert payload's Data, and is shared with other sinks.
func TestTemplateVarsDoesNotMutateData(t *testing.T) {
	data := map[string]interface{}{"temp": 95.0}
	_ = TemplateVars(data, "sensors", "r", "c", "", 0)

	if len(data) != 1 {
		t.Fatalf("record was mutated: %v", data)
	}
	if _, exists := data["store"]; exists {
		t.Error("TemplateVars leaked built-ins into the caller's record")
	}
}

// The staleness example from the issue.
func TestStalenessTemplate(t *testing.T) {
	data := map[string]interface{}{
		"last_timestamp":  int64(1747000000000000000),
		"age_seconds":     612.0,
		"max_age_seconds": 300.0,
		"store":           "nas-syn-002-disks",
		"rule_type":       "staleness",
	}
	vars := TemplateVars(data, "nas-syn-002-disks", "disks quiet", "no data for 5m0s", "", 1747000000000000000)

	tmpl := "{store} went quiet — no data for {age_seconds}s (limit {max_age_seconds}s)"
	want := "nas-syn-002-disks went quiet — no data for 612s (limit 300s)"
	if got := RenderMessage(tmpl, vars); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestValidateMessageTemplate(t *testing.T) {
	if err := ValidateMessageTemplate(""); err != nil {
		t.Errorf("empty template rejected: %v", err)
	}
	if err := ValidateMessageTemplate("temp is {temp}"); err != nil {
		t.Errorf("valid template rejected: %v", err)
	}
	if err := ValidateMessageTemplate(strings.Repeat("a", MaxMessageTemplateBytes)); err != nil {
		t.Errorf("template at the cap rejected: %v", err)
	}
	if err := ValidateMessageTemplate(strings.Repeat("a", MaxMessageTemplateBytes+1)); err == nil {
		t.Error("oversized template accepted")
	}
	if err := ValidateMessageTemplate("bad\x00null"); err == nil {
		t.Error("template with a null byte accepted")
	}
	// Unknown fields are a render-time concern: the field set is not
	// known for JSON stores, so create-time validation would either
	// reject valid templates or give false confidence.
	if err := ValidateMessageTemplate("{definitely_not_a_field}"); err != nil {
		t.Errorf("unknown field rejected at validation time: %v", err)
	}
}

// Rendering is per-fire, and fires are rare, but a template referencing
// many fields should not be pathological.
func BenchmarkRenderMessage(b *testing.B) {
	vars := TemplateVars(map[string]interface{}{
		"temp": 95.5, "humidity": 30.0, "sensor_id": "abc-1",
	}, "sensors", "high temp", "temperature > 80", "ref", 1747000000000000000)
	tmpl := "{store}: {sensor_id} temp {temp:.1f} humidity {humidity:.0f} at {timestamp:time}"

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RenderMessage(tmpl, vars)
	}
}

func nan() float64 { var z float64; return z / z }
func inf() float64 { var z float64; return 1 / z }
