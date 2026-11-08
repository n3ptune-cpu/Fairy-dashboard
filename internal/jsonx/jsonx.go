// Package jsonx has small accessors for decoded JSON (map[string]any), mirroring Python's dict.get semantics:
// a missing key or a value of the wrong type reads as nil / zero instead of an error. It is used for payloads
// from systems Fairy does not control (Proxmox, agents, devices).
package jsonx

import (
	"encoding/json"
	"math"
)

// M is a decoded JSON object.
type M = map[string]any

// Obj returns m[k] as an object (nil if absent or not an object).
func Obj(m M, k string) M {
	v, _ := m[k].(map[string]any)
	return v
}

// List returns m[k] as an array (nil if absent or not an array).
func List(m M, k string) []any {
	v, _ := m[k].([]any)
	return v
}

// Num returns a number (bools are not numbers, like the Python _num check).
func Num(m M, k string) *float64 {
	if m == nil {
		return nil
	}
	return NumV(m[k])
}

// NumV returns v as a number (nil for non-numbers, bools, NaN and infinities).
func NumV(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return &x
	case int:
		f := float64(x)
		return &f
	case int64:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

// Int returns m[k] as an int64 (fractions truncated), or nil.
func Int(m M, k string) *int64 {
	f := Num(m, k)
	if f == nil {
		return nil
	}
	i := int64(*f)
	return &i
}

// IntP returns m[k] as an int (fractions truncated), or nil.
func IntP(m M, k string) *int {
	f := Num(m, k)
	if f == nil {
		return nil
	}
	i := int(*f)
	return &i
}

// Str returns a non-empty string value or nil.
func Str(m M, k string) *string {
	if m == nil {
		return nil
	}
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}

// S returns m[k] as a string, or "" when it is absent or not a string.
func S(m M, k string) string {
	if p := Str(m, k); p != nil {
		return *p
	}
	return ""
}

// Bool returns m[k] as a bool, or def when it is absent or not a bool.
func Bool(m M, k string, def bool) bool {
	if m == nil {
		return def
	}
	if b, ok := m[k].(bool); ok {
		return b
	}
	return def
}

// Truthy mirrors Python truthiness for JSON values.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Objs returns the objects in a JSON array, skipping elements that are not objects. It returns an empty
// (non-nil) slice when v is not an array.
func Objs(v any) []M {
	l, _ := v.([]any)
	out := make([]M, 0, len(l))
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
