// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the Business Source License 1.1
// See LICENSE file for details.

// Package rules provides a simple rules engine for evaluating conditions
// against data records. Supports field comparisons and logical operators.
package rules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Operator represents a comparison operator.
type Operator string

const (
	OpEq  Operator = "=="
	OpNe  Operator = "!="
	OpGt  Operator = ">"
	OpGe  Operator = ">="
	OpLt  Operator = "<"
	OpLe  Operator = "<="
)

// Condition represents a single field comparison.
type Condition struct {
	Field    string
	Operator Operator
	Value    interface{} // string, float64, or bool
}

// Rule represents a parsed rule with one or more conditions.
type Rule struct {
	Name       string
	Conditions []Condition
	LogicalOp  string // "AND" or "OR", empty means single condition
}

// conditionPattern matches: field operator value
// Examples: "temperature > 80", "status == \"error\"", "count >= 100"
var conditionPattern = regexp.MustCompile(`^\s*(\w+)\s*(==|!=|>=|<=|>|<)\s*(.+)\s*$`)

// Parse parses a condition string into a Rule.
// Supports:
//   - Simple: "temperature > 80"
//   - AND: "temperature > 80 AND humidity < 30"
//   - OR: "status == \"error\" OR status == \"critical\""
func Parse(name, condition string) (*Rule, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return nil, fmt.Errorf("empty condition")
	}

	rule := &Rule{Name: name}

	// Check for logical operators (case-insensitive)
	upperCond := strings.ToUpper(condition)

	var parts []string
	if strings.Contains(upperCond, " AND ") {
		rule.LogicalOp = "AND"
		parts = splitKeepingQuotes(condition, " AND ", " and ")
	} else if strings.Contains(upperCond, " OR ") {
		rule.LogicalOp = "OR"
		parts = splitKeepingQuotes(condition, " OR ", " or ")
	} else {
		parts = []string{condition}
	}

	for _, part := range parts {
		cond, err := parseCondition(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid condition %q: %w", part, err)
		}
		rule.Conditions = append(rule.Conditions, cond)
	}

	return rule, nil
}

// splitKeepingQuotes splits on delimiters but respects quoted strings.
func splitKeepingQuotes(s string, delims ...string) []string {
	// Simple approach: find delimiter positions outside quotes
	upper := strings.ToUpper(s)
	for _, delim := range delims {
		upperDelim := strings.ToUpper(delim)
		idx := strings.Index(upper, upperDelim)
		if idx != -1 {
			// Check if inside quotes (simple heuristic)
			beforeQuotes := strings.Count(s[:idx], "\"")
			if beforeQuotes%2 == 0 {
				left := s[:idx]
				right := s[idx+len(delim):]
				result := []string{left}
				result = append(result, splitKeepingQuotes(right, delims...)...)
				return result
			}
		}
	}
	return []string{s}
}

// parseCondition parses a single condition like "temperature > 80".
func parseCondition(s string) (Condition, error) {
	matches := conditionPattern.FindStringSubmatch(s)
	if matches == nil {
		return Condition{}, fmt.Errorf("cannot parse: expected 'field operator value'")
	}

	field := matches[1]
	op := Operator(matches[2])
	valueStr := strings.TrimSpace(matches[3])

	value, err := parseValue(valueStr)
	if err != nil {
		return Condition{}, err
	}

	return Condition{
		Field:    field,
		Operator: op,
		Value:    value,
	}, nil
}

// parseValue parses a value string into the appropriate type.
func parseValue(s string) (interface{}, error) {
	s = strings.TrimSpace(s)

	// Check for quoted string
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], nil
	}

	// Check for boolean
	lower := strings.ToLower(s)
	if lower == "true" {
		return true, nil
	}
	if lower == "false" {
		return false, nil
	}

	// Try to parse as number
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}

	// Treat as unquoted string
	return s, nil
}

// Evaluate evaluates the rule against a data record.
// Returns true if the rule fires (condition is met).
func (r *Rule) Evaluate(data map[string]interface{}) bool {
	if len(r.Conditions) == 0 {
		return false
	}

	if len(r.Conditions) == 1 {
		return r.Conditions[0].Evaluate(data)
	}

	if r.LogicalOp == "AND" {
		for _, cond := range r.Conditions {
			if !cond.Evaluate(data) {
				return false
			}
		}
		return true
	}

	// OR
	for _, cond := range r.Conditions {
		if cond.Evaluate(data) {
			return true
		}
	}
	return false
}

// Evaluate evaluates a single condition against a data record.
func (c *Condition) Evaluate(data map[string]interface{}) bool {
	fieldValue, ok := data[c.Field]
	if !ok {
		return false
	}

	return compare(fieldValue, c.Operator, c.Value)
}

// compare compares two values using the given operator.
func compare(fieldValue interface{}, op Operator, ruleValue interface{}) bool {
	// Convert field value to float64 if possible for numeric comparison
	fieldFloat, fieldIsNum := toFloat64(fieldValue)
	ruleFloat, ruleIsNum := toFloat64(ruleValue)

	// Numeric comparison
	if fieldIsNum && ruleIsNum {
		switch op {
		case OpEq:
			return fieldFloat == ruleFloat
		case OpNe:
			return fieldFloat != ruleFloat
		case OpGt:
			return fieldFloat > ruleFloat
		case OpGe:
			return fieldFloat >= ruleFloat
		case OpLt:
			return fieldFloat < ruleFloat
		case OpLe:
			return fieldFloat <= ruleFloat
		}
	}

	// String comparison
	fieldStr := toString(fieldValue)
	ruleStr := toString(ruleValue)

	switch op {
	case OpEq:
		return fieldStr == ruleStr
	case OpNe:
		return fieldStr != ruleStr
	case OpGt:
		return fieldStr > ruleStr
	case OpGe:
		return fieldStr >= ruleStr
	case OpLt:
		return fieldStr < ruleStr
	case OpLe:
		return fieldStr <= ruleStr
	}

	return false
}

// toFloat64 attempts to convert a value to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int16:
		return float64(n), true
	case int8:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint8:
		return float64(n), true
	}
	return 0, false
}

// toString converts a value to string.
func toString(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case bool:
		if s {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}
