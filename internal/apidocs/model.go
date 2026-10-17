package apidocs

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
)

// TypeRef is how a type is shown; Link points at a schema section when the
// type is a named schema.
type TypeRef struct{ Label, Link string }

// FieldDoc is a property or parameter row.
type FieldDoc struct {
	Name        string
	In          string // parameters: path, query, header
	Type        TypeRef
	Required    bool
	Description template.HTML
	Constraints string
}

// BodyDoc is a request or response body.
type BodyDoc struct {
	ContentType string
	Type        TypeRef
	Fields      []FieldDoc
	Example     string
}

// RespDoc is one response of an operation.
type RespDoc struct {
	Status      string
	Description template.HTML
	Body        *BodyDoc
}

// OpDoc is one endpoint.
type OpDoc struct {
	ID, Anchor, Method, Path, Summary string
	Description                       template.HTML
	Params                            []FieldDoc
	Request                           *BodyDoc
	Responses                         []RespDoc
}

// Group is the endpoints of one resource.
type Group struct {
	Name, Anchor string
	Ops          []OpDoc
}

// SchemaDoc is one component schema.
type SchemaDoc struct {
	Name, Anchor string
	Description  template.HTML
	Kind         string // object, enum, ...
	Fields       []FieldDoc
	Example      string
}

// Page is everything the template needs. AppName, LogoURL and Accent are the
// fields the shared layout reads.
type Page struct {
	AppName, LogoURL, Accent string

	Title, Version, Summary string
	Intro                   template.HTML
	ServerURL               string
	Auth                    template.HTML
	Groups                  []Group
	Schemas                 []SchemaDoc
	OpCount                 int
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func anchor(prefix, s string) string { return prefix + unsafeID.ReplaceAllString(s, "-") }

// Page builds the reference.
func (d *Doc) Page() Page {
	info := d.root.mapOf("info")
	p := Page{Title: info.str("title"), Version: fmt.Sprint(info.get("version")), Summary: info.str("summary"), Intro: markdown(info.str("description"))}
	if servers := d.root.list("servers"); len(servers) > 0 {
		if s, ok := servers[0].(*omap); ok {
			url := s.str("url")
			if vars := s.mapOf("variables"); vars != nil {
				for _, k := range vars.keys {
					if v, ok := vars.vals[k].(*omap); ok {
						url = strings.ReplaceAll(url, "{"+k+"}", fmt.Sprint(v.get("default")))
					}
				}
			}
			p.ServerURL = url
		}
	}
	if sec := d.root.mapOf("components").mapOf("securitySchemes"); sec != nil {
		var parts []string
		for _, k := range sec.keys {
			if s, ok := sec.vals[k].(*omap); ok {
				parts = append(parts, "<strong>"+html.EscapeString(k)+"</strong>: "+html.EscapeString(s.str("type")+" "+s.str("scheme"))+" "+string(markdown(s.str("description"))))
			}
		}
		p.Auth = template.HTML(strings.Join(parts, "<br>"))
	}

	paths := d.root.mapOf("paths")
	groupIdx := map[string]int{}
	for _, path := range paths.keys {
		item, _ := paths.vals[path].(*omap)
		shared := item.list("parameters")
		for _, method := range item.keys {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
			default:
				continue
			}
			op, _ := item.vals[method].(*omap)
			if op == nil {
				continue
			}
			od := d.operation(strings.ToUpper(method), path, op, shared)
			g := resourceOf(path)
			i, ok := groupIdx[g]
			if !ok {
				i = len(p.Groups)
				groupIdx[g] = i
				p.Groups = append(p.Groups, Group{Name: g, Anchor: anchor("group-", g)})
			}
			p.Groups[i].Ops = append(p.Groups[i].Ops, od)
			p.OpCount++
		}
	}
	if schemas := d.root.mapOf("components").mapOf("schemas"); schemas != nil {
		for _, name := range schemas.keys {
			p.Schemas = append(p.Schemas, d.schemaDoc(name, schemas.vals[name]))
		}
	}
	return p
}

// resourceOf is the group of a path: "/v1/payment_links/{id}" -> "payment_links".
func resourceOf(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 1 && regexp.MustCompile(`^v\d+$`).MatchString(parts[0]) {
		return parts[1]
	}
	return parts[0]
}

func (d *Doc) operation(method, path string, op *omap, shared []any) OpDoc {
	id := op.str("operationId")
	if id == "" {
		id = strings.ToLower(method) + "-" + path
	}
	od := OpDoc{ID: id, Anchor: anchor("op-", id), Method: method, Path: path, Summary: op.str("summary"), Description: markdown(op.str("description"))}
	for _, raw := range append(append([]any{}, shared...), op.list("parameters")...) {
		v, _ := d.resolve(raw)
		pm, ok := v.(*omap)
		if !ok {
			continue
		}
		f := FieldDoc{Name: pm.str("name"), In: pm.str("in"), Required: pm.get("required") == true || pm.str("in") == "path",
			Description: markdown(pm.str("description"))}
		if sch := pm.get("schema"); sch != nil {
			f.Type = d.typeOf(sch)
			f.Constraints = d.constraints(sch)
		}
		od.Params = append(od.Params, f)
	}
	if rb, _ := d.resolveMap(op.get("requestBody")); rb != nil {
		od.Request = d.body(rb.mapOf("content"), "")
	}
	if rs := op.mapOf("responses"); rs != nil {
		for _, status := range rs.keys {
			rm, _ := d.resolveMap(rs.vals[status])
			if rm == nil {
				continue
			}
			rd := RespDoc{Status: status, Description: markdown(rm.str("description"))}
			rd.Body = d.body(rm.mapOf("content"), "")
			od.Responses = append(od.Responses, rd)
		}
	}
	return od
}

func (d *Doc) resolveMap(v any) (*omap, string) {
	r, ref := d.resolve(v)
	m, _ := r.(*omap)
	return m, ref
}

// body describes the first content type of a content map.
func (d *Doc) body(content *omap, _ string) *BodyDoc {
	if content == nil || len(content.keys) == 0 {
		return nil
	}
	ct := content.keys[0]
	media, _ := content.vals[ct].(*omap)
	if media == nil {
		return nil
	}
	b := &BodyDoc{ContentType: ct}
	sch := media.get("schema")
	if sch == nil {
		return b
	}
	b.Type = d.typeOf(sch)
	if m, _ := d.resolveMap(sch); m != nil {
		b.Fields = d.fieldsOf(m, 0)
		if ex := media.get("example"); ex != nil {
			b.Example = toJSON(ex)
		} else if ex := d.example(sch, "", nil, 0); ex != nil {
			b.Example = toJSON(ex)
		}
	}
	return b
}

func (d *Doc) schemaDoc(name string, schema any) SchemaDoc {
	m, _ := schema.(*omap)
	sd := SchemaDoc{Name: name, Anchor: anchor("schema-", name)}
	if m == nil {
		return sd
	}
	sd.Description = markdown(m.str("description"))
	sd.Kind = typeName(m)
	if len(m.list("allOf")) > 0 {
		sd.Kind = "object"
	}
	if en := m.list("enum"); len(en) > 0 {
		sd.Kind = "enum"
		var vals []string
		for _, e := range en {
			vals = append(vals, fmt.Sprint(e))
		}
		sd.Description = markdown(m.str("description")) + oneOf(vals)
		return sd
	}
	sd.Fields = d.fieldsOf(m, 0)
	if ex := d.example(schema, "", []string{name}, 0); ex != nil {
		sd.Example = toJSON(ex)
	}
	return sd
}

func oneOf(vals []string) template.HTML {
	esc := make([]string, len(vals))
	for i, v := range vals {
		esc[i] = "<code>" + html.EscapeString(v) + "</code>"
	}
	return template.HTML("<p>One of: " + strings.Join(esc, ", ") + "</p>")
}

// fieldsOf lists the properties of an object schema, following allOf and $ref.
func (d *Doc) fieldsOf(m *omap, depth int) []FieldDoc {
	if depth > 4 || m == nil {
		return nil
	}
	var out []FieldDoc
	for _, part := range m.list("allOf") {
		if pm, _ := d.resolveMap(part); pm != nil {
			out = append(out, d.fieldsOf(pm, depth+1)...)
		}
	}
	required := map[string]bool{}
	for _, r := range m.list("required") {
		if s, ok := r.(string); ok {
			required[s] = true
		}
	}
	if props := m.mapOf("properties"); props != nil {
		for _, name := range props.keys {
			sch := props.vals[name]
			pm, _ := d.resolveMap(sch)
			f := FieldDoc{Name: name, Required: required[name], Type: d.typeOf(sch), Constraints: d.constraints(sch)}
			if pm != nil {
				f.Description = markdown(pm.str("description"))
			}
			if sm, ok := sch.(*omap); ok && sm.str("description") != "" {
				f.Description = markdown(sm.str("description"))
			}
			out = append(out, f)
		}
	}
	return out
}

// typeOf describes a schema in a few words.
func (d *Doc) typeOf(schema any) TypeRef {
	m, ok := schema.(*omap)
	if !ok {
		return TypeRef{Label: "any"}
	}
	if ref := m.str("$ref"); ref != "" {
		n := refName(ref)
		if strings.Contains(ref, "/schemas/") {
			return TypeRef{Label: n, Link: "#" + anchor("schema-", n)}
		}
		return TypeRef{Label: n}
	}
	if all := m.list("allOf"); len(all) > 0 {
		var names []string
		link := ""
		for _, a := range all {
			t := d.typeOf(a)
			names = append(names, t.Label)
			if link == "" {
				link = t.Link
			}
		}
		return TypeRef{Label: strings.Join(names, " + "), Link: link}
	}
	label := ""
	switch t := m.get("type").(type) {
	case string:
		label = t
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, fmt.Sprint(e))
		}
		label = strings.Join(parts, " or ")
	}
	switch label {
	case "array":
		it := d.typeOf(m.get("items"))
		return TypeRef{Label: "array of " + it.Label, Link: it.Link}
	case "object":
		if ap := m.mapOf("additionalProperties"); ap != nil {
			it := d.typeOf(ap)
			return TypeRef{Label: "map of " + it.Label, Link: it.Link}
		}
	}
	if f := m.str("format"); f != "" {
		label += " (" + f + ")"
	}
	if label == "" {
		if m.has("const") {
			return TypeRef{Label: "const"}
		}
		return TypeRef{Label: "any"}
	}
	return TypeRef{Label: label}
}

// constraints lists the limits of a schema: enum, range, length, pattern, default.
func (d *Doc) constraints(schema any) string {
	m, _ := d.resolveMap(schema)
	if m == nil {
		return ""
	}
	var parts []string
	if c := m.get("const"); c != nil {
		parts = append(parts, "always "+fmt.Sprint(c))
	}
	if en := m.list("enum"); len(en) > 0 {
		var vals []string
		for _, e := range en {
			vals = append(vals, fmt.Sprint(e))
		}
		parts = append(parts, "one of "+strings.Join(vals, ", "))
	}
	for _, k := range [][2]string{{"minimum", "min"}, {"maximum", "max"}, {"minLength", "min length"}, {"maxLength", "max length"},
		{"minItems", "min items"}, {"maxItems", "max items"}, {"maxProperties", "max keys"}} {
		if m.has(k[0]) {
			parts = append(parts, fmt.Sprintf("%s %v", k[1], m.get(k[0])))
		}
	}
	if p := m.str("pattern"); p != "" {
		parts = append(parts, "pattern "+p)
	}
	if m.has("default") && m.get("default") != nil {
		parts = append(parts, fmt.Sprintf("default %v", m.get("default")))
	}
	return strings.Join(parts, "; ")
}

// ---- tiny markdown: paragraphs, "- " and "1. " lists, **bold**, `code` ----

var (
	boldRE   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	listRE   = regexp.MustCompile(`^(?:[-*]|\d+\.)\s+`)
	orderedR = regexp.MustCompile(`^\d+\.\s+`)
)

// inline escapes s and then applies code spans and bold, so markup in the
// source can never become markup in the page.
func inline(s string) string {
	parts := strings.Split(s, "`")
	for i, p := range parts {
		esc := html.EscapeString(p)
		if i%2 == 1 && i < len(parts)-1 {
			parts[i] = "<code>" + esc + "</code>"
			continue
		}
		esc = boldRE.ReplaceAllString(esc, "<strong>$1</strong>")
		if i%2 == 1 { // an unmatched backtick: keep it
			esc = "`" + esc
		}
		parts[i] = esc
	}
	return strings.Join(parts, "")
}

func markdown(s string) template.HTML {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" {
		return ""
	}
	var out strings.Builder
	for _, block := range regexp.MustCompile(`\n\s*\n`).Split(s, -1) {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if listRE.MatchString(strings.TrimSpace(lines[0])) {
			tag := "ul"
			if orderedR.MatchString(strings.TrimSpace(lines[0])) {
				tag = "ol"
			}
			var items []string
			for _, ln := range lines {
				t := strings.TrimSpace(ln)
				if listRE.MatchString(t) {
					items = append(items, listRE.ReplaceAllString(t, ""))
				} else if len(items) > 0 {
					items[len(items)-1] += " " + t
				}
			}
			out.WriteString("<" + tag + ">")
			for _, it := range items {
				out.WriteString("<li>" + inline(it) + "</li>")
			}
			out.WriteString("</" + tag + ">\n")
			continue
		}
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		out.WriteString("<p>" + inline(strings.Join(lines, " ")) + "</p>\n")
	}
	return template.HTML(out.String())
}
