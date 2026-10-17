// Package apidocs turns docs/openapi.yaml into a self-contained HTML
// reference page: endpoints, parameters, schemas and examples, rendered on the
// server with the shared layout and no JavaScript.
package apidocs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// omap is a YAML mapping that remembers the order of its keys, so the page
// follows the order of the spec.
type omap struct {
	keys []string
	vals map[string]any
}

func (m *omap) get(k string) any {
	if m == nil {
		return nil
	}
	return m.vals[k]
}

func (m *omap) str(k string) string {
	s, _ := m.get(k).(string)
	return s
}

func (m *omap) mapOf(k string) *omap {
	v, _ := m.get(k).(*omap)
	return v
}

func (m *omap) list(k string) []any {
	v, _ := m.get(k).([]any)
	return v
}

func (m *omap) has(k string) bool {
	if m == nil {
		return false
	}
	_, ok := m.vals[k]
	return ok
}

// convert turns a yaml node into strings, numbers, bools, nil, []any and *omap.
func convert(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return convert(n.Content[0])
	case yaml.MappingNode:
		m := &omap{vals: map[string]any{}}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			v, err := convert(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			if _, dup := m.vals[k]; !dup {
				m.keys = append(m.keys, k)
			}
			m.vals[k] = v
		}
		return m, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := convert(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.AliasNode:
		return convert(n.Alias)
	default:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	}
}

// Doc is a parsed OpenAPI document.
type Doc struct {
	root *omap
}

// Parse reads an OpenAPI 3.x document.
func Parse(raw []byte) (*Doc, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("apidocs: %w", err)
	}
	v, err := convert(&n)
	if err != nil {
		return nil, fmt.Errorf("apidocs: %w", err)
	}
	root, ok := v.(*omap)
	if !ok || root.mapOf("paths") == nil || len(root.mapOf("paths").keys) == 0 {
		return nil, errors.New("apidocs: the document has no paths")
	}
	if root.mapOf("info") == nil {
		return nil, errors.New("apidocs: the document has no info")
	}
	return &Doc{root: root}, nil
}

// resolve follows a local $ref ("#/components/schemas/X"); other values are returned as they are.
func (d *Doc) resolve(v any) (any, string) {
	m, ok := v.(*omap)
	if !ok {
		return v, ""
	}
	ref := m.str("$ref")
	if ref == "" {
		return v, ""
	}
	cur := any(d.root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		cm, ok := cur.(*omap)
		if !ok {
			return nil, ref
		}
		cur = cm.get(strings.NewReplacer("~1", "/", "~0", "~").Replace(part))
	}
	return cur, ref
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// ---- ordered JSON ----

// writeJSON writes v as indented JSON, keeping the order of *omap keys.
func writeJSON(buf *bytes.Buffer, v any, indent string) {
	switch x := v.(type) {
	case *omap:
		if len(x.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i, k := range x.keys {
			kb, _ := json.Marshal(k)
			buf.WriteString(indent + "  " + string(kb) + ": ")
			writeJSON(buf, x.vals[k], indent+"  ")
			if i < len(x.keys)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, e := range x {
			buf.WriteString(indent + "  ")
			writeJSON(buf, e, indent+"  ")
			if i < len(x)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(indent + "]")
	default:
		b, err := json.Marshal(x)
		if err != nil {
			b = []byte("null")
		}
		buf.Write(b)
	}
}

func toJSON(v any) string {
	var buf bytes.Buffer
	writeJSON(&buf, v, "")
	return buf.String()
}
