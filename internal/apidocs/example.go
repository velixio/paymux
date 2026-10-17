package apidocs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const maxExampleDepth = 6

// ExampleJSON builds an example document for the named component schema.
func (d *Doc) ExampleJSON(schema string) (string, error) {
	s := d.root.mapOf("components").mapOf("schemas")
	if s == nil || !s.has(schema) {
		return "", fmt.Errorf("apidocs: no schema %q", schema)
	}
	v := d.example(s.get(schema), "", nil, 0)
	if v == nil {
		return "", fmt.Errorf("apidocs: schema %q has no example", schema)
	}
	return toJSON(v), nil
}

var idPatternRE = regexp.MustCompile(`^\^([a-z]+_)\[[^\]]+\]\{(\d+)\}\$$`)

const idFill = "01hxyzabcdefghjkmnpqrstvw0"

// example builds a sample value of a schema. An explicit `example`, `const`,
// `default` or first `enum` value wins; otherwise the value follows the type,
// the format and a few well-known property names. stack stops recursion
// through self-referencing schemas.
func (d *Doc) example(schema any, prop string, stack []string, depth int) any {
	if depth > maxExampleDepth {
		return nil
	}
	resolved, ref := d.resolve(schema)
	if ref != "" {
		name := refName(ref)
		for _, s := range stack {
			if s == name {
				return nil
			}
		}
		stack = append(stack[:len(stack):len(stack)], name)
	}
	m, ok := resolved.(*omap)
	if !ok {
		return nil
	}
	if m.has("example") {
		return m.get("example")
	}
	if m.has("const") {
		return m.get("const")
	}
	if all := m.list("allOf"); len(all) > 0 {
		merged := &omap{vals: map[string]any{}}
		for _, part := range all {
			if sub, ok := d.example(part, prop, stack, depth+1).(*omap); ok {
				for _, k := range sub.keys {
					if _, dup := merged.vals[k]; !dup {
						merged.keys = append(merged.keys, k)
					}
					merged.vals[k] = sub.vals[k]
				}
			}
		}
		return merged
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if opts := m.list(key); len(opts) > 0 {
			return d.example(opts[0], prop, stack, depth+1)
		}
	}
	if en := m.list("enum"); len(en) > 0 {
		return en[0]
	}
	if m.has("default") && m.get("default") != nil {
		return m.get("default")
	}
	switch typeName(m) {
	case "object":
		out := &omap{vals: map[string]any{}}
		if props := m.mapOf("properties"); props != nil {
			for _, k := range props.keys {
				if v := d.example(props.vals[k], k, stack, depth+1); v != nil {
					out.keys = append(out.keys, k)
					out.vals[k] = v
				}
			}
		} else if ap := m.mapOf("additionalProperties"); ap != nil {
			if v := d.example(ap, "key", stack, depth+1); v != nil {
				out.keys, out.vals["key"] = []string{"key"}, v
			}
		}
		return out
	case "array":
		if v := d.example(m.get("items"), prop, stack, depth+1); v != nil {
			return []any{v}
		}
		return []any{}
	case "string":
		return exampleString(m, prop)
	case "integer":
		return exampleInt(m, prop)
	case "number":
		return 1.5
	case "boolean":
		return true
	}
	// A schema without a type but with properties is an object.
	if m.mapOf("properties") != nil {
		return d.example(withType(m, "object"), prop, stack, depth+1)
	}
	return nil
}

func withType(m *omap, t string) *omap {
	c := &omap{keys: append([]string{"type"}, m.keys...), vals: map[string]any{"type": t}}
	for k, v := range m.vals {
		c.vals[k] = v
	}
	return c
}

// typeName is the schema's type, ignoring "null" in a type list.
func typeName(m *omap) string {
	switch t := m.get("type").(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s, _ := e.(string); s != "null" && s != "" {
				return s
			}
		}
	}
	return ""
}

func exampleString(m *omap, prop string) any {
	if pat := m.str("pattern"); pat != "" {
		if g := idPatternRE.FindStringSubmatch(pat); g != nil {
			n, _ := strconv.Atoi(g[2])
			return g[1] + strings.Repeat(idFill, n/len(idFill)+1)[:n]
		}
	}
	switch m.str("format") {
	case "date-time":
		return "2026-10-17T12:00:00Z"
	case "date":
		return "2026-10-17"
	case "uri", "url":
		return "https://example.com/path"
	case "email":
		return "customer@example.com"
	case "binary":
		return "(binary)"
	}
	switch {
	case prop == "currency":
		return "INR"
	case prop == "description":
		return "Pro plan"
	case strings.HasSuffix(prop, "_url"):
		return "https://example.com/path"
	case prop == "email":
		return "customer@example.com"
	case prop == "name":
		return "Asha K"
	}
	return "string"
}

func exampleInt(m *omap, prop string) any {
	switch {
	case strings.Contains(prop, "amount") || prop == "total" || prop == "taxable":
		return 49900
	case prop == "version":
		return 1
	}
	if n, ok := m.get("minimum").(int); ok && n > 0 {
		return n
	}
	return 1
}
