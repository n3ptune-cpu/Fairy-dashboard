// Package jsonx: small accessors for decoded JSON (map[string]any), mirroring Python's dict.get semantics.
package jsonx

import (
	"encoding/json"
	"math"
)

type M = map[string]any

// Obj returns m[k] as an object (nil if absent or not an object).
func Obj(m M, k string) M {
	v, _ := m[k].(map[string]any)
	return v
}

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

func Int(m M, k string) *int64 {
	f := Num(m, k)
	if f == nil {
		return nil
	}
	i := int64(*f)
	return &i
}

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

func S(m M, k string) string {
	if p := Str(m, k); p != nil {
		return *p
	}
	return ""
}

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
