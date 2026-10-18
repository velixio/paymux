// Package redact removes credentials and customer personal data from text
// before it is stored in the logs table. Three layers, strongest first:
//
//  1. Known values. The redactor is told the secrets in use (provider keys,
//     the master key, webhook, SMTP and OIDC secrets, ...) and replaces every
//     exact occurrence, raw or URL- or JSON-escaped, wherever it appears.
//  2. Field names. JSON, form and query fields whose name is a credential
//     (password, token, api_key, signature, ...) or personal data (email,
//     phone, name, address, vpa, gstin, ...) lose their value. Names are matched
//     by whole words, so payment_id and key_id stay readable.
//  3. Value patterns, as a backstop for what has no telling name: provider key
//     formats, JWTs, bearer values, PEM blocks, card numbers (Luhn), e-mail
//     addresses, phone numbers, GSTIN, PAN and UPI handles.
//
// The functions are deterministic and safe for concurrent use.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Replacement values.
const (
	Secret = "[redacted]"
	PII    = "[pii]"
)

// MinKnownLen is the shortest known value that is replaced: shorter ones
// would mangle ordinary text.
const MinKnownLen = 6

// Redactor holds the known secret values and applies all three layers.
type Redactor struct {
	mu     sync.Mutex
	owners map[string][]string
	repl   atomic.Pointer[strings.Replacer]
}

// New returns a Redactor that knows no secret values yet.
func New() *Redactor { return &Redactor{owners: map[string][]string{}} }

// SetKnown replaces the secret values registered by owner (for instance
// "account:pa_123"), so an account's secrets can change without a restart.
func (r *Redactor) SetKnown(owner string, values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(values) == 0 {
		delete(r.owners, owner)
	} else {
		r.owners[owner] = append([]string(nil), values...)
	}
	r.rebuild()
}

// Forget drops the values registered by owner.
func (r *Redactor) Forget(owner string) { r.SetKnown(owner) }

// Known returns how many distinct values are being replaced.
func (r *Redactor) Known() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.distinct())
}

func (r *Redactor) distinct() []string {
	seen := map[string]bool{}
	var out []string
	for _, vs := range r.owners {
		for _, v := range vs {
			if len(v) >= MinKnownLen && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

func (r *Redactor) rebuild() {
	vals := r.distinct()
	if len(vals) == 0 {
		r.repl.Store(nil)
		return
	}
	variants := map[string]bool{}
	for _, v := range vals {
		variants[v] = true
		variants[url.QueryEscape(v)] = true
		variants[url.PathEscape(v)] = true
		if b, err := json.Marshal(v); err == nil && len(b) > 2 {
			variants[string(b[1:len(b)-1])] = true
		}
		variants[strings.ReplaceAll(strings.ReplaceAll(v, "&", "&amp;"), "<", "&lt;")] = true
	}
	list := make([]string, 0, len(variants))
	for v := range variants {
		if len(v) >= MinKnownLen {
			list = append(list, v)
		}
	}
	// Longest first: when one secret contains another, the longer one wins.
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	pairs := make([]string, 0, 2*len(list))
	for _, v := range list {
		pairs = append(pairs, v, Secret)
	}
	r.repl.Store(strings.NewReplacer(pairs...))
}

func (r *Redactor) known(s string) string {
	if p := r.repl.Load(); p != nil {
		return p.Replace(s)
	}
	return s
}

// ---- value patterns ----

type pattern struct {
	re   *regexp.Regexp
	with string
}

var credPatterns = []pattern{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?(?:-----END [A-Z0-9 ]+-----|$)`), Secret},
	{regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9_]{8,}`), Secret},
	{regexp.MustCompile(`\brzp_(?:live|test)_[A-Za-z0-9_]{6,}`), Secret},
	{regexp.MustCompile(`\bwhsec_[A-Za-z0-9+/=_-]{8,}`), Secret},
	{regexp.MustCompile(`\bws_[A-Za-z0-9+/=_-]{8,}`), Secret},
	{regexp.MustCompile(`\bapik_[A-Za-z0-9_-]{6,}`), Secret},
	{regexp.MustCompile(`\bcfsk_[A-Za-z0-9_-]{6,}`), Secret},
	{regexp.MustCompile(`\bpmx_(?:live|test)_[A-Za-z0-9_]{6,}`), Secret},
	{regexp.MustCompile(`\bses_[A-Za-z0-9_-]{16,}`), Secret},
	{regexp.MustCompile(`\b1000\.[0-9a-f]{16,}\.[0-9a-f]{16,}`), Secret},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), Secret},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`), "Bearer " + Secret},
	{regexp.MustCompile(`\bBasic\s+[A-Za-z0-9+/]{12,}={0,2}`), "Basic " + Secret},
	{regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Secret + "@"},
}

var (
	reGSTIN = regexp.MustCompile(`\b\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`)
	rePAN   = regexp.MustCompile(`\b[A-Z]{5}\d{4}[A-Z]\b`)
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	reVPA   = regexp.MustCompile(`[A-Za-z0-9._-]{2,64}@[A-Za-z][A-Za-z0-9]{1,31}`)
	rePhone = regexp.MustCompile(`(?:\+91[\s-]?|\b91[\s-]?|\b0)?\b[6-9]\d{9}\b`)
	reIntl  = regexp.MustCompile(`\+\d[\d\s-]{8,14}\d`)
	reCard  = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	reKV    = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9_.-]{0,63})(\\?["']?\s*[:=]\s*\\?["']?)([^\s"',;&}\]\\]+)`)
)

func (r *Redactor) patterns(s string) string {
	for _, p := range credPatterns {
		s = p.re.ReplaceAllString(s, p.with)
	}
	s = reCard.ReplaceAllStringFunc(s, maskCard)
	s = reGSTIN.ReplaceAllString(s, "[gstin]")
	s = rePAN.ReplaceAllString(s, "[pan]")
	s = reEmail.ReplaceAllString(s, "[email]")
	s = replaceVPA(s)
	s = rePhone.ReplaceAllString(s, "[phone]")
	s = reIntl.ReplaceAllString(s, "[phone]")
	return s
}

// replaceVPA masks UPI handles (name@bank) that are not the start of a
// dotted host name, which the e-mail pattern has already handled.
func replaceVPA(s string) string {
	idx := reVPA.FindAllStringIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		end := m[1]
		if end < len(s) && (s[end] == '.' || s[end] == '-') && end+1 < len(s) && isAlnum(s[end+1]) {
			continue // user@host.tld: not a handle
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("[vpa]")
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// maskCard masks a Luhn-valid 13 to 19 digit run, keeping the last four.
func maskCard(m string) string {
	digits := make([]byte, 0, len(m))
	for i := 0; i < len(m); i++ {
		if m[i] >= '0' && m[i] <= '9' {
			digits = append(digits, m[i])
		}
	}
	// No payment card starts with 1; 13-digit values starting with 1 are
	// millisecond timestamps.
	if len(digits) < 13 || digits[0] == '1' || !luhn(digits) {
		return m
	}
	return "[card ****" + string(digits[len(digits)-4:]) + "]"
}

func luhn(d []byte) bool {
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// kv replaces the value of "name=value" and "name: value" pairs in free text
// whose name is a credential (and, with pii, personal data).
func kv(s string, pii bool) string { return kvDepth(s, pii, 0) }

func kvDepth(s string, pii bool, depth int) string {
	return reKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := reKV.FindStringSubmatch(m)
		key, sep, val := sub[1], sub[2], sub[3]
		if strings.HasPrefix(val, "[") {
			return m
		}
		switch {
		case isCredentialName(key):
			return key + sep + Secret
		case pii && isPIIName(key):
			return key + sep + PII
		}
		// The value may hold more pairs ("error: password=x", "https://h/p?t=x").
		if depth < 3 && strings.ContainsAny(val, "=:") {
			return key + sep + kvDepth(val, pii, depth+1)
		}
		return m
	})
}

// Scrub replaces only the known secret values, leaving everything else as it
// is. For text that was redacted when it was stored and is being shown again:
// a secret that became known since is still hidden, and a JSON document stays
// valid JSON.
func (r *Redactor) Scrub(s string) string { return r.known(s) }

// Text redacts free text: log messages, error strings, URLs.
func (r *Redactor) Text(s string) string {
	if s == "" {
		return s
	}
	s = r.known(s)
	s = r.patterns(s)
	return kv(s, false)
}

// Error redacts an error's text.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.Text(err.Error())
}

// Cap shortens s to at most n bytes (on a character boundary), marking the cut.
func Cap(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "...[truncated]"
	cut := n - len(mark)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// Body redacts a request or response body. JSON is walked field by field;
// form bodies are parsed; anything else (including JSON cut short by a size
// limit) is scrubbed as text, with field-name pairs found by pattern.
func (r *Redactor) Body(contentType string, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := r.known(string(b))
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "{") || strings.HasPrefix(t, "["):
		if out, ok := r.jsonWalk(t); ok {
			return out
		}
	case strings.Contains(strings.ToLower(contentType), "x-www-form-urlencoded") || looksLikeForm(t):
		if out, ok := r.formWalk(t); ok {
			return out
		}
	}
	return kv(r.patterns(s), true)
}

func looksLikeForm(t string) bool {
	return strings.Contains(t, "=") && !strings.ContainsAny(t, " \n\t{}") && !strings.HasPrefix(t, "http")
}

func (r *Redactor) formWalk(t string) (string, bool) {
	vals, err := url.ParseQuery(t)
	if err != nil || len(vals) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range vals[k] {
			parts = append(parts, url.QueryEscape(k)+"="+r.fieldValue(k, v))
		}
	}
	return strings.Join(parts, "&"), true
}

// fieldValue is the redacted text of one named string value.
func (r *Redactor) fieldValue(key, v string) string {
	switch {
	case isCredentialName(key) || queryCredential[strings.ToLower(key)]:
		return Secret
	case isPIIName(key):
		return PII
	}
	return url.QueryEscape(r.patternsText(v))
}

// patternsText is Text without the known-value pass (already applied).
func (r *Redactor) patternsText(s string) string { return kv(r.patterns(s), false) }

func (r *Redactor) jsonWalk(t string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if dec.More() {
		return "", false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.walk("", v, false, 0)); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

func (r *Redactor) walk(key string, v any, inCard bool, depth int) any {
	if depth > 24 {
		return Secret
	}
	if key != "" {
		switch {
		case isCredentialName(key):
			return Secret
		case isPIIName(key):
			return PII
		case inCard && cardField[strings.ToLower(key)]:
			return Secret
		}
	}
	switch x := v.(type) {
	case map[string]any:
		card := inCard || hasWord(key, "card", "cards")
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = r.walk(k, e, card, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.walk(key, e, inCard, depth+1)
		}
		return out
	case string:
		t := strings.TrimSpace(x)
		if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			if s, ok := r.jsonWalk(t); ok { // JSON inside a string
				return s
			}
		}
		return r.patternsText(x)
	case json.Number:
		if inCard || hasWord(key, "card") {
			return Secret
		}
		return x
	}
	return v
}

var cardField = map[string]bool{"number": true, "num": true, "exp_month": true, "exp_year": true, "expiry": true, "expiry_month": true,
	"expiry_year": true, "holder": true}

func hasWord(key string, ws ...string) bool {
	for _, w := range words(key) {
		for _, c := range ws {
			if w == c {
				return true
			}
		}
	}
	return false
}

// allowedHeader says which headers are stored at all.
func allowedHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "content-type", "content-length", "user-agent", "accept", "retry-after", "date", "server":
		return true
	}
	return strings.Contains(n, "request-id") || strings.Contains(n, "request_id") || strings.Contains(n, "correlation-id") ||
		strings.Contains(n, "trace-id") || strings.HasPrefix(n, "x-ratelimit") || n == "cf-ray"
}

// Headers returns the allowlisted headers only, with redacted values.
func (r *Redactor) Headers(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		if allowedHeader(k) && len(vs) > 0 {
			out[http.CanonicalHeaderKey(k)] = r.Text(strings.Join(vs, ", "))
		}
	}
	return out
}

// URL redacts a URL: user info is dropped, secret-named and personal query
// fields lose their values, and the rest goes through Text.
func (r *Redactor) URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return r.Text(raw)
	}
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme + "://")
	}
	b.WriteString(u.Host)
	b.WriteString(r.Text(u.EscapedPath()))
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(r.Query(u.RawQuery))
	}
	return b.String()
}

// Query redacts a raw query string.
func (r *Redactor) Query(raw string) string {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return r.Text(raw)
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range vals[k] {
			parts = append(parts, url.QueryEscape(k)+"="+r.queryValue(k, v))
		}
	}
	return strings.Join(parts, "&")
}

func (r *Redactor) queryValue(key, v string) string {
	switch {
	case isCredentialName(key) || queryCredential[strings.ToLower(key)]:
		return Secret
	case isPIIName(key):
		return PII
	}
	return url.QueryEscape(r.Text(v))
}

// queryCredential are parameter names that are credentials in a query string
// or a form (OAuth grant codes and states, one-time codes) though the same
// names are harmless elsewhere (an error "code" in a JSON body).
var queryCredential = map[string]bool{"code": true, "state": true, "nonce": true, "sid": true, "session": true, "sessionid": true,
	"authcode": true, "auth_code": true, "oauth_verifier": true, "code_verifier": true}

// Value redacts one named log attribute.
func (r *Redactor) Value(key string, v any) any {
	switch {
	case isCredentialName(key):
		return Secret
	case isPIIName(key):
		return PII
	}
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return r.Text(x)
	case error:
		return r.Text(x.Error())
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return x
	case json.RawMessage:
		return r.Body("application/json", x)
	}
	return r.Text(fmt.Sprint(v))
}

// IsCredentialName and IsPIIName expose the field-name rules.
func IsCredentialName(key string) bool { return isCredentialName(key) }
func IsPIIName(key string) bool        { return isPIIName(key) }
