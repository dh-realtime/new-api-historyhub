package historyhub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// This file implements the N204 response-body merge for SSE streams.
//
// Rule (02.md): consecutive "data: <json>" lines whose JSON differs in EXACTLY
// ONE field value — same field names, same field count, all other values equal —
// are merged by CONCATENATING that one differing (string) field's value. Blank
// lines are dropped. "data: [DONE]" and non-data lines are emitted verbatim and
// act as group boundaries. Only string-valued differences are merged; a single
// non-string difference (e.g. a number, or finish_reason null→"stop") leaves the
// line unmerged. Object KEY ORDER is preserved (we parse with json.Decoder, not
// into a map), so the merged output matches the upstream chunk layout.

// ordJSON is an order-preserving JSON value: exactly one of scalar / object /
// array is set. scalar holds the raw bytes of a string/number/bool/null.
type ordJSON struct {
	scalar []byte
	keys   []string
	vals   map[string]*ordJSON
	arr    []*ordJSON
}

func parseOrd(data []byte) (*ordJSON, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return parseOrdToken(dec)
}

func parseOrdToken(dec *json.Decoder) (*ordJSON, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return buildOrd(dec, tok)
}

func buildOrd(dec *json.Decoder, tok json.Token) (*ordJSON, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			// keys starts as a NON-nil empty slice so an empty object {}
			// marshals back as "{}" (a nil slice would fall through every
			// marshal case and render as nothing).
			o := &ordJSON{keys: []string{}, vals: map[string]*ordJSON{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := parseOrdToken(dec)
				if err != nil {
					return nil, err
				}
				o.keys = append(o.keys, key)
				o.vals[key] = v
			}
			if _, err := dec.Token(); err != nil { // closing }
				return nil, err
			}
			return o, nil
		case '[':
			// arr starts non-nil for the same reason as keys above: an empty
			// array [] must marshal back as "[]".
			o := &ordJSON{arr: []*ordJSON{}}
			for dec.More() {
				v, err := parseOrdToken(dec)
				if err != nil {
					return nil, err
				}
				o.arr = append(o.arr, v)
			}
			if _, err := dec.Token(); err != nil { // closing ]
				return nil, err
			}
			return o, nil
		}
		return nil, fmt.Errorf("unexpected delim %v", t)
	case string:
		b, _ := json.Marshal(t)
		return &ordJSON{scalar: b}, nil
	case json.Number:
		return &ordJSON{scalar: []byte(string(t))}, nil
	case bool:
		if t {
			return &ordJSON{scalar: []byte("true")}, nil
		}
		return &ordJSON{scalar: []byte("false")}, nil
	case nil:
		return &ordJSON{scalar: []byte("null")}, nil
	}
	return nil, fmt.Errorf("unexpected token %T", tok)
}

func (o *ordJSON) marshal(buf *strings.Builder) {
	switch {
	case o.scalar != nil:
		buf.Write(o.scalar)
	case o.keys != nil:
		buf.WriteByte('{')
		for i, k := range o.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			o.vals[k].marshal(buf)
		}
		buf.WriteByte('}')
	case o.arr != nil:
		buf.WriteByte('[')
		for i, v := range o.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			v.marshal(buf)
		}
		buf.WriteByte(']')
	}
}

// mergeFrom merges src into o if they differ in exactly one STRING-valued leaf
// (concatenating that leaf). o is mutated in place when it returns true.
func (o *ordJSON) mergeFrom(src *ordJSON) bool {
	// Scalars have no parent here to splice into; only object/array children merge.
	if o.scalar != nil || src.scalar != nil {
		return false
	}
	if o.keys != nil && src.keys != nil {
		if len(o.keys) != len(src.keys) {
			return false
		}
		for _, k := range o.keys {
			if _, ok := src.vals[k]; !ok {
				return false
			}
		}
		diffKey := ""
		diffs := 0
		for _, k := range o.keys {
			if !ordEqual(o.vals[k], src.vals[k]) {
				diffs++
				if diffs > 1 {
					return false
				}
				diffKey = k
			}
		}
		if diffs != 1 {
			return false
		}
		a, b := o.vals[diffKey], src.vals[diffKey]
		if a.mergeFrom(b) {
			return true
		}
		return concatScalars(a, b)
	}
	if o.arr != nil && src.arr != nil {
		if len(o.arr) != len(src.arr) {
			return false
		}
		diffIdx := -1
		for i := range o.arr {
			if !ordEqual(o.arr[i], src.arr[i]) {
				if diffIdx != -1 {
					return false
				}
				diffIdx = i
			}
		}
		if diffIdx == -1 {
			return false
		}
		a, b := o.arr[diffIdx], src.arr[diffIdx]
		if a.mergeFrom(b) {
			return true
		}
		return concatScalars(a, b)
	}
	return false
}

// concatScalars concatenates two scalar string leaves in place on a. Returns
// false if either isn't a JSON string.
func concatScalars(a, b *ordJSON) bool {
	if a.scalar == nil || b.scalar == nil {
		return false
	}
	if !isJSONString(a.scalar) || !isJSONString(b.scalar) {
		return false
	}
	var sa, sb string
	if json.Unmarshal(a.scalar, &sa) != nil || json.Unmarshal(b.scalar, &sb) != nil {
		return false
	}
	merged, err := json.Marshal(sa + sb)
	if err != nil {
		return false
	}
	a.scalar = merged
	return true
}

func isJSONString(b []byte) bool {
	return len(b) >= 2 && b[0] == '"'
}

func ordEqual(a, b *ordJSON) bool {
	if a.scalar != nil || b.scalar != nil {
		if a.scalar == nil || b.scalar == nil {
			return false
		}
		return bytes.Equal(a.scalar, b.scalar)
	}
	if a.keys != nil || b.keys != nil {
		if a.keys == nil || b.keys == nil {
			return false
		}
		if len(a.keys) != len(b.keys) {
			return false
		}
		for i, k := range a.keys {
			if k != b.keys[i] {
				return false
			}
			if !ordEqual(a.vals[k], b.vals[k]) {
				return false
			}
		}
		return true
	}
	if a.arr != nil || b.arr != nil {
		if a.arr == nil || b.arr == nil {
			return false
		}
		if len(a.arr) != len(b.arr) {
			return false
		}
		for i := range a.arr {
			if !ordEqual(a.arr[i], b.arr[i]) {
				return false
			}
		}
		return true
	}
	return true
}

// mergeSSELines applies the N204 data-line merge to an SSE (or any) response
// body: consecutive "data: <json>" lines differing in one string field are
// merged (value concatenated), blank lines dropped, all else emitted verbatim.
func mergeSSELines(buf []byte) string {
	var out strings.Builder
	var cur *ordJSON
	flush := func() {
		if cur != nil {
			out.WriteString("data: ")
			cur.marshal(&out)
			out.WriteByte('\n')
			cur = nil
		}
	}
	for _, raw := range strings.Split(string(buf), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue // no blank lines
		}
		const p = "data: "
		if !strings.HasPrefix(line, p) {
			flush()
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		payload := line[len(p):]
		if payload == "[DONE]" {
			flush()
			out.WriteString(p + "[DONE]\n")
			continue
		}
		node, err := parseOrd([]byte(payload))
		if err != nil {
			flush()
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		if cur != nil && cur.mergeFrom(node) {
			continue // merged into cur
		}
		flush()
		cur = node
	}
	flush()
	return out.String()
}
