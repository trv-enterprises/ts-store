// Copyright (c) 2026 TRV Enterprises LLC
// SPDX-License-Identifier: Apache-2.0
// See LICENSE file for details.

package rules

import (
	"testing"
)

func TestParse_SimpleConditions(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		wantErr   bool
		wantField string
		wantOp    Operator
	}{
		{"greater than", "temperature > 80", false, "temperature", OpGt},
		{"greater equal", "temperature >= 80", false, "temperature", OpGe},
		{"less than", "temperature < 80", false, "temperature", OpLt},
		{"less equal", "temperature <= 80", false, "temperature", OpLe},
		{"equal", "status == \"error\"", false, "status", OpEq},
		{"not equal", "status != \"ok\"", false, "status", OpNe},
		{"with spaces", "  count  >=  100  ", false, "count", OpGe},
		{"dotted field", "temp.cpu_max_core_c > 75", false, "temp.cpu_max_core_c", OpGt},
		{"hyphen field", "disk-usage > 90", false, "disk-usage", OpGt},
		{"empty", "", true, "", ""},
		{"invalid no op", "temperature 80", true, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if len(rule.Conditions) != 1 {
				t.Errorf("expected 1 condition, got %d", len(rule.Conditions))
				return
			}
			if rule.Conditions[0].Field != tt.wantField {
				t.Errorf("Field = %v, want %v", rule.Conditions[0].Field, tt.wantField)
			}
			if rule.Conditions[0].Operator != tt.wantOp {
				t.Errorf("Operator = %v, want %v", rule.Conditions[0].Operator, tt.wantOp)
			}
		})
	}
}

func TestParse_CompoundConditions(t *testing.T) {
	tests := []struct {
		name       string
		condition  string
		wantCount  int
		wantLogOp  string
	}{
		{"AND", "temperature > 80 AND humidity < 30", 2, "AND"},
		{"and lowercase", "temperature > 80 and humidity < 30", 2, "AND"},
		{"OR", "status == \"error\" OR status == \"critical\"", 2, "OR"},
		{"or lowercase", "status == \"error\" or status == \"critical\"", 2, "OR"},
		{"triple AND", "a > 1 AND b > 2 AND c > 3", 3, "AND"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if err != nil {
				t.Errorf("Parse() error = %v", err)
				return
			}
			if len(rule.Conditions) != tt.wantCount {
				t.Errorf("condition count = %d, want %d", len(rule.Conditions), tt.wantCount)
			}
			if rule.LogicalOp != tt.wantLogOp {
				t.Errorf("LogicalOp = %v, want %v", rule.LogicalOp, tt.wantLogOp)
			}
		})
	}
}

func TestParse_Values(t *testing.T) {
	tests := []struct {
		condition string
		wantValue interface{}
	}{
		{"x > 80", float64(80)},
		{"x > 80.5", float64(80.5)},
		{"x == \"hello\"", "hello"},
		{"x == true", true},
		{"x == false", false},
		{"x == -10", float64(-10)},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if err != nil {
				t.Errorf("Parse() error = %v", err)
				return
			}
			got := rule.Conditions[0].Value
			if got != tt.wantValue {
				t.Errorf("Value = %v (%T), want %v (%T)", got, got, tt.wantValue, tt.wantValue)
			}
		})
	}
}

func TestEvaluate_Numeric(t *testing.T) {
	tests := []struct {
		condition string
		data      map[string]interface{}
		want      bool
	}{
		{"temperature > 80", map[string]interface{}{"temperature": 85.0}, true},
		{"temperature > 80", map[string]interface{}{"temperature": 80.0}, false},
		{"temperature > 80", map[string]interface{}{"temperature": 75.0}, false},
		{"temperature >= 80", map[string]interface{}{"temperature": 80.0}, true},
		{"temperature < 80", map[string]interface{}{"temperature": 75.0}, true},
		{"temperature <= 80", map[string]interface{}{"temperature": 80.0}, true},
		{"temperature == 80", map[string]interface{}{"temperature": 80.0}, true},
		{"temperature != 80", map[string]interface{}{"temperature": 85.0}, true},
		// Integer values
		{"count > 10", map[string]interface{}{"count": 15}, true},
		{"count > 10", map[string]interface{}{"count": int64(15)}, true},
		// Dotted/hyphenated field names (flat keys with literal dots, as produced by system-stats)
		{"temp.cpu_max_core_c > 75", map[string]interface{}{"temp.cpu_max_core_c": 82.0}, true},
		{"temp.cpu_max_core_c > 75", map[string]interface{}{"temp.cpu_max_core_c": 70.0}, false},
		{"disk-usage > 90", map[string]interface{}{"disk-usage": 95.0}, true},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			got := rule.Evaluate(tt.data)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_String(t *testing.T) {
	tests := []struct {
		condition string
		data      map[string]interface{}
		want      bool
	}{
		{"status == \"error\"", map[string]interface{}{"status": "error"}, true},
		{"status == \"error\"", map[string]interface{}{"status": "ok"}, false},
		{"status != \"error\"", map[string]interface{}{"status": "ok"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			got := rule.Evaluate(tt.data)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_Contains(t *testing.T) {
	tests := []struct {
		condition string
		data      map[string]interface{}
		want      bool
	}{
		{"message contains \"ERROR\"", map[string]interface{}{"message": "ERROR: something failed"}, true},
		{"message contains \"ERROR\"", map[string]interface{}{"message": "error: something failed"}, false}, // case-sensitive
		{"message contains \"ERROR\"", map[string]interface{}{"message": "all good"}, false},
		{"message contains \"fail\"", map[string]interface{}{"message": "Operation failed successfully"}, true},
		{"log contains \"WARNING\"", map[string]interface{}{"log": "2026-02-10 WARNING: low disk"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			rule, err := Parse("test", tt.condition)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			got := rule.Evaluate(tt.data)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_MissingField(t *testing.T) {
	rule, _ := Parse("test", "temperature > 80")
	data := map[string]interface{}{"humidity": 50.0}

	if rule.Evaluate(data) {
		t.Error("expected false for missing field")
	}
}

func TestEvaluate_AND(t *testing.T) {
	rule, _ := Parse("test", "temperature > 80 AND humidity < 30")

	tests := []struct {
		name string
		data map[string]interface{}
		want bool
	}{
		{"both true", map[string]interface{}{"temperature": 85.0, "humidity": 25.0}, true},
		{"first false", map[string]interface{}{"temperature": 75.0, "humidity": 25.0}, false},
		{"second false", map[string]interface{}{"temperature": 85.0, "humidity": 35.0}, false},
		{"both false", map[string]interface{}{"temperature": 75.0, "humidity": 35.0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rule.Evaluate(tt.data)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluate_OR(t *testing.T) {
	rule, _ := Parse("test", "status == \"error\" OR status == \"critical\"")

	tests := []struct {
		name string
		data map[string]interface{}
		want bool
	}{
		{"first true", map[string]interface{}{"status": "error"}, true},
		{"second true", map[string]interface{}{"status": "critical"}, true},
		{"neither", map[string]interface{}{"status": "ok"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rule.Evaluate(tt.data)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Regression tests for issue #26: the parser silently mis-parsed trailing
// garbage and mixed AND/OR, and numeric strings compared lexicographically.

func TestParseRejectsTrailingGarbage(t *testing.T) {
	for _, cond := range []string{
		"temperature > 80 extra",
		`status == "err" extra`,
		"temperature > 80 humidity < 30", // forgotten AND
	} {
		if _, err := Parse("r", cond); err == nil {
			t.Errorf("Parse(%q) accepted trailing garbage", cond)
		}
	}
}

func TestParseRejectsMixedAndOr(t *testing.T) {
	for _, cond := range []string{
		"a > 1 OR b < 2 AND c == 3",
		"a > 1 AND b < 2 OR c == 3",
	} {
		if _, err := Parse("r", cond); err == nil {
			t.Errorf("Parse(%q) accepted mixed AND/OR", cond)
		}
	}
}

func TestParseLogicalWordsInsideQuotes(t *testing.T) {
	// "and"/"or" inside a quoted value must not influence operator
	// selection or trip the mixed-operator check.
	r, err := Parse("r", `msg contains "up and down" OR temp > 5`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.LogicalOp != "OR" || len(r.Conditions) != 2 {
		t.Fatalf("expected OR with 2 conditions, got %q with %d", r.LogicalOp, len(r.Conditions))
	}
	if !r.Evaluate(map[string]interface{}{"msg": "going up and down", "temp": 1.0}) {
		t.Error("contains-condition with quoted 'and' did not match")
	}
}

func TestNumericStringsCompareNumerically(t *testing.T) {
	cases := []struct {
		cond  string
		data  map[string]interface{}
		want  bool
		descr string
	}{
		{"temp > 100", map[string]interface{}{"temp": "90"}, false, `"90" > 100 lexicographic false-fire`},
		{"temp > 80", map[string]interface{}{"temp": "600.5"}, true, `"600.5" > 80 lexicographic false-miss`},
		{"temp == 80", map[string]interface{}{"temp": "80"}, true, `"80" == 80 numeric equality`},
		{"temp < 200", map[string]interface{}{"temp": "90"}, true, `"90" < 200`},
	}
	for _, c := range cases {
		r, err := Parse("r", c.cond)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.cond, err)
		}
		if got := r.Evaluate(c.data); got != c.want {
			t.Errorf("%s: Evaluate(%q, %v) = %v, want %v", c.descr, c.cond, c.data, got, c.want)
		}
	}
}

func TestParseStillAcceptsValidConditions(t *testing.T) {
	for _, cond := range []string{
		"temperature > 80",
		"temperature > 80 AND humidity < 30",
		`status == "error" OR status == "critical"`,
		`message contains "disk full"`,
		"active == true",
		"cpu.percent >= 99.5",
		"level == warn", // bare single-token string value
	} {
		if _, err := Parse("r", cond); err != nil {
			t.Errorf("Parse(%q) rejected a valid condition: %v", cond, err)
		}
	}
}
