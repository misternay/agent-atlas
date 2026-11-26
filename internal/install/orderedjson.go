package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Value is an order-preserving JSON value so hook installation leaves keys of
// unrelated settings in their original order.
type Value struct {
	Obj  *Object
	Arr  []*Value
	Raw  json.RawMessage // scalars (string, number, bool, null)
	kind byte            // 'o', 'a', 's'
}

// Object is an ordered JSON object.
type Object struct {
	Keys []string
	Vals map[string]*Value
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{Vals: map[string]*Value{}} }

// Get returns a member.
func (o *Object) Get(k string) (*Value, bool) { v, ok := o.Vals[k]; return v, ok }

// Set sets a member, appending new keys at the end.
func (o *Object) Set(k string, v *Value) {
	if _, ok := o.Vals[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.Vals[k] = v
}

// Delete removes a member.
func (o *Object) Delete(k string) {
	if _, ok := o.Vals[k]; !ok {
		return
	}
	delete(o.Vals, k)
	for i, kk := range o.Keys {
		if kk == k {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			break
		}
	}
}

// ObjVal wraps an object.
func ObjVal(o *Object) *Value { return &Value{Obj: o, kind: 'o'} }

// ArrVal wraps an array.
func ArrVal(a []*Value) *Value { return &Value{Arr: a, kind: 'a'} }

// StrVal wraps a string scalar.
func StrVal(s string) *Value { return &Value{Raw: marshalNoEscape(s), kind: 's'} }

// marshalNoEscape encodes without HTML escaping so shell commands such as
// "a && b > c" in unrelated hooks keep their readable form.
func marshalNoEscape(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// NumVal wraps an integer scalar.
func NumVal(n int) *Value { return &Value{Raw: json.RawMessage(fmt.Sprint(n)), kind: 's'} }

// IsObj reports whether v is an object.
func (v *Value) IsObj() bool { return v != nil && v.kind == 'o' }

// IsArr reports whether v is an array.
func (v *Value) IsArr() bool { return v != nil && v.kind == 'a' }

// Str returns the string value of a string scalar.
func (v *Value) Str() string {
	if v == nil || v.kind != 's' {
		return ""
	}
	var s string
	_ = json.Unmarshal(v.Raw, &s)
	return s
}

// Parse decodes JSON preserving object key order.
func Parse(b []byte) (*Value, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (*Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return ObjVal(o), nil
		case '[':
			var arr []*Value
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if arr == nil {
				arr = []*Value{}
			}
			return ArrVal(arr), nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case json.Number:
		return &Value{Raw: json.RawMessage(t.String()), kind: 's'}, nil
	default:
		return &Value{Raw: marshalNoEscape(t), kind: 's'}, nil
	}
}

// Marshal encodes with two-space indentation and a trailing newline.
func Marshal(v *Value) []byte {
	var b strings.Builder
	write(&b, v, 0)
	b.WriteString("\n")
	return []byte(b.String())
}

func write(b *strings.Builder, v *Value, depth int) {
	ind := strings.Repeat("  ", depth)
	switch v.kind {
	case 'o':
		if len(v.Obj.Keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range v.Obj.Keys {
			kb := marshalNoEscape(k)
			b.WriteString(ind + "  ")
			b.Write(kb)
			b.WriteString(": ")
			write(b, v.Obj.Vals[k], depth+1)
			if i < len(v.Obj.Keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(ind + "}")
	case 'a':
		if len(v.Arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range v.Arr {
			b.WriteString(ind + "  ")
			write(b, e, depth+1)
			if i < len(v.Arr)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(ind + "]")
	default:
		b.Write(v.Raw)
	}
}
