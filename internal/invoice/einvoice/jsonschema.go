package einvoice

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed schema/einvoice-1.1.json
var schemaJSON []byte

// SchemaJSON returns the committed JSON Schema documents are validated against.
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

// A small JSON Schema (draft-07) checker, enough for the keywords the e-invoice
// schema uses: type, enum, pattern, minLength/maxLength, minimum/maximum,
// required, properties, additionalProperties:false, items, minItems/maxItems,
// allOf, local $ref, and Paymux's x-maxDecimals. It is not a general validator.

type schema = map[string]any

var (
	compiled     schema
	compileOnce  sync.Once
	compileErr   error
	patternCache sync.Map
)

func rootSchema() (schema, error) {
	compileOnce.Do(func() {
		dec := json.NewDecoder(bytes.NewReader(schemaJSON))
		dec.UseNumber()
		compileErr = dec.Decode(&compiled)
	})
	return compiled, compileErr
}

func pattern(p string) (*regexp.Regexp, error) {
	if re, ok := patternCache.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}

type checker struct {
	root schema
	errs []string
}

func (c *checker) fail(path, format string, a ...any) {
	if path == "" {
		path = "document"
	}
	c.errs = append(c.errs, path+": "+fmt.Sprintf(format, a...))
}

func (c *checker) resolve(ref string) schema {
	cur := any(c.root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(schema)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	s, _ := cur.(schema)
	return s
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		_ = x
		return "unknown"
	}
}

func isInteger(n json.Number) bool {
	r, ok := new(big.Rat).SetString(string(n))
	return ok && r.IsInt()
}

func typeMatches(want string, v any) bool {
	t := typeOf(v)
	switch want {
	case "integer":
		n, ok := v.(json.Number)
		return ok && isInteger(n)
	case "number":
		return t == "number"
	}
	return t == want
}

func decimals(n json.Number) int {
	s := string(n)
	if strings.ContainsAny(s, "eE") {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return 99
		}
		// Count digits of the shortest exact decimal.
		for d := 0; d < 20; d++ {
			scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil)))
			if scaled.IsInt() {
				return d
			}
		}
		return 99
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(strings.TrimRight(s[i+1:], "0"))
	}
	return 0
}

func (c *checker) check(v any, s schema, path string) {
	if ref, ok := s["$ref"].(string); ok {
		if target := c.resolve(ref); target != nil {
			c.check(v, target, path)
		} else {
			c.fail(path, "schema reference %s cannot be resolved", ref)
		}
		return
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, part := range all {
			if ps, ok := part.(schema); ok {
				c.check(v, ps, path)
			}
		}
	}
	if t, ok := s["type"]; ok {
		var types []string
		switch tt := t.(type) {
		case string:
			types = []string{tt}
		case []any:
			for _, e := range tt {
				types = append(types, fmt.Sprint(e))
			}
		}
		match := false
		for _, want := range types {
			match = match || typeMatches(want, v)
		}
		if !match {
			c.fail(path, "must be %s, not %s", strings.Join(types, " or "), typeOf(v))
			return
		}
	}
	if en, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			c.fail(path, "%v is not one of %v", v, en)
		}
	}
	switch x := v.(type) {
	case string:
		n := utf8.RuneCountInString(x)
		if m, ok := s["minLength"].(json.Number); ok {
			if lo, _ := m.Int64(); int64(n) < lo {
				c.fail(path, "is shorter than %d characters", lo)
			}
		}
		if m, ok := s["maxLength"].(json.Number); ok {
			if hi, _ := m.Int64(); int64(n) > hi {
				c.fail(path, "is longer than %d characters", hi)
			}
		}
		if p, ok := s["pattern"].(string); ok {
			if re, err := pattern(p); err != nil {
				c.fail(path, "schema pattern %q is invalid: %v", p, err)
			} else if !re.MatchString(x) {
				c.fail(path, "%q does not match the pattern %s", x, p)
			}
		}
	case json.Number:
		r, ok := new(big.Rat).SetString(string(x))
		if !ok {
			c.fail(path, "is not a number")
			break
		}
		if m, ok := s["minimum"].(json.Number); ok {
			if lo, _ := new(big.Rat).SetString(string(m)); lo != nil && r.Cmp(lo) < 0 {
				c.fail(path, "%s is below the minimum %s", x, m)
			}
		}
		if m, ok := s["maximum"].(json.Number); ok {
			if hi, _ := new(big.Rat).SetString(string(m)); hi != nil && r.Cmp(hi) > 0 {
				c.fail(path, "%s is above the maximum %s", x, m)
			}
		}
		if m, ok := s["x-maxDecimals"].(json.Number); ok {
			if d, _ := m.Int64(); int64(decimals(x)) > d {
				c.fail(path, "%s has more than %d decimal places", x, d)
			}
		}
	case map[string]any:
		props, _ := s["properties"].(schema)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, has := x[fmt.Sprint(r)]; !has {
					c.fail(join(path, fmt.Sprint(r)), "is required")
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(schema); ok {
				c.check(x[k], ps, join(path, k))
			} else if ap, ok := s["additionalProperties"].(bool); ok && !ap {
				c.fail(join(path, k), "is not a field of this schema")
			}
		}
	case []any:
		if m, ok := s["minItems"].(json.Number); ok {
			if lo, _ := m.Int64(); int64(len(x)) < lo {
				c.fail(path, "needs at least %d entries", lo)
			}
		}
		if m, ok := s["maxItems"].(json.Number); ok {
			if hi, _ := m.Int64(); int64(len(x)) > hi {
				c.fail(path, "has more than %d entries", hi)
			}
		}
		if is, ok := s["items"].(schema); ok {
			for i, e := range x {
				c.check(e, is, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}
