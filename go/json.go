// Ordered JSON values with JSON.stringify-compatible escaping.
//
// The Node implementation builds responses as plain objects whose key order
// is insertion order (selection order for GraphQL, literal order for REST).
// Go maps marshal with sorted keys, which would reorder fields, so this
// file carries a small ordered value type and a marshaller whose string
// escaping matches JSON.stringify: only ", \, and control characters are
// escaped (\b \f \n \r \t, else \u00XX). No HTML escaping.

package main

import (
	"strconv"
	"strings"
)

// JVal is one JSON value.
type JVal struct {
	kind byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	obj  []JPair
	arr  []*JVal
	str  string
	num  string
	b    bool
}

// JPair is one object member, in order.
type JPair struct {
	Key string
	Val *JVal
}

func JObj(pairs ...JPair) *JVal  { return &JVal{kind: 'o', obj: pairs} }
func JArr(items ...*JVal) *JVal  { return &JVal{kind: 'a', arr: items} }
func JStr(s string) *JVal        { return &JVal{kind: 's', str: s} }
func JNum(n int64) *JVal         { return &JVal{kind: 'n', num: strconv.FormatInt(n, 10)} }
func JNumF(f float64) *JVal      { return &JVal{kind: 'n', num: strconv.FormatFloat(f, 'g', -1, 64)} }
func JBool(b bool) *JVal         { return &JVal{kind: 'b', b: b} }
func JNull() *JVal               { return &JVal{kind: 'z'} }

// JString marshals s as a JSON string literal, JSON.stringify style.
func JString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Marshal renders the value as compact JSON.
func (v *JVal) Marshal() string {
	var b strings.Builder
	v.write(&b)
	return b.String()
}

func (v *JVal) write(b *strings.Builder) {
	switch v.kind {
	case 'o':
		b.WriteByte('{')
		for i, p := range v.obj {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(JString(p.Key))
			b.WriteByte(':')
			p.Val.write(b)
		}
		b.WriteByte('}')
	case 'a':
		b.WriteByte('[')
		for i, item := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			item.write(b)
		}
		b.WriteByte(']')
	case 's':
		b.WriteString(JString(v.str))
	case 'n':
		b.WriteString(v.num)
	case 'b':
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case 'z':
		b.WriteString("null")
	}
}

// JField is a convenience for building an object member from a Go value.
func JField(key string, val any) JPair {
	return JPair{Key: key, Val: JAny(val)}
}

// JAny converts a Go value: *JVal passes through, string/bool/int/float64/
// nil map to their JSON forms, []any and map[string]any are NOT accepted
// (use JObj/JArr to keep field order explicit).
func JAny(val any) *JVal {
	switch t := val.(type) {
	case *JVal:
		return t
	case string:
		return JStr(t)
	case bool:
		return JBool(t)
	case int:
		return JNum(int64(t))
	case int64:
		return JNum(t)
	case float64:
		return JNumF(t)
	case nil:
		return JNull()
	default:
		return JStr("")
	}
}
