package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/velixio/paymux/internal/settings"
)

// describe turns a resolved setting into a row with the control that suits
// it. The server still validates everything in settings.Set.
func describe(def settings.Def, val settings.Value) settingRow {
	m, ok := settings.MetaFor(def.Key)
	if !ok {
		m = settings.Meta{Kind: settings.KindJSON}
	}
	row := settingRow{Label: settingLabel(def.Key), Group: settingGroup(def.Key), Key: def.Key, Source: string(val.Source), Overridden: val.Source == settings.SourceApp, Help: m.Help, Kind: string(m.Kind), Unit: m.Unit,
		Pattern: m.Pattern}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, val.Raw, "", "  "); err != nil {
		pretty.Reset()
		pretty.Write(val.Raw)
	}
	row.Value = pretty.String()

	text, display := textOf(m, val.Raw)
	row.Text, row.Display = text, withUnit(m, display)
	_, defDisplay := textOf(m, json.RawMessage(def.Default))
	row.Default = withUnit(m, defDisplay)
	if m.Kind == settings.KindJSON {
		row.Default = compactTruncated(def.Default, 90)
	}
	switch m.Kind {
	case settings.KindEnum:
		var cur string
		_ = json.Unmarshal(val.Raw, &cur)
		for _, o := range m.Options {
			row.Options = append(row.Options, enumOpt{Value: o, Selected: o == cur})
		}
	case settings.KindBool:
		_ = json.Unmarshal(val.Raw, &row.Checked)
	case settings.KindInt, settings.KindFloat:
		lo := m.Min
		if m.ZeroOK {
			lo = 0
		}
		row.Min = strconv.FormatFloat(lo, 'f', -1, 64)
		if m.Max != 0 {
			row.Max = strconv.FormatFloat(m.Max, 'f', -1, 64)
		}
		row.Step = "1"
		if m.Kind == settings.KindFloat {
			row.Step = "any"
		}
		if m.Unit == "paise" {
			var n int64
			if json.Unmarshal(val.Raw, &n) == nil {
				row.Aux = "₹" + formatMinor2(n)
			}
		}
	}
	return row
}

// textOf returns the text a control holds and the plain-words display.
func textOf(m settings.Meta, raw json.RawMessage) (text, display string) {
	switch m.Kind {
	case settings.KindBool:
		var b bool
		_ = json.Unmarshal(raw, &b)
		if b {
			return "true", "on"
		}
		return "false", "off"
	case settings.KindEnum, settings.KindString, settings.KindClock:
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, s
	case settings.KindInt, settings.KindFloat:
		s := strings.TrimSpace(string(raw))
		return s, s
	case settings.KindList:
		var items []json.Number
		_ = json.Unmarshal(raw, &items)
		parts := make([]string, len(items))
		for i, n := range items {
			parts[i] = n.String()
		}
		s := strings.Join(parts, ", ")
		if s == "" {
			return "", "none"
		}
		return s, s
	}
	return string(raw), string(raw)
}

func withUnit(m settings.Meta, display string) string {
	if m.Unit == "" || display == "" || display == "none" || m.Kind == settings.KindBool || m.Kind == settings.KindEnum {
		return display
	}
	return display + " " + m.Unit
}

func compactTruncated(raw string, n int) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(raw)) != nil {
		return raw
	}
	s := b.String()
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// formatMinor2 prints paise as rupees with Indian digit grouping: 1500000 -> "15,000.00".
func formatMinor2(paise int64) string {
	return strings.TrimPrefix(formatRupees(paise), "₹")
}

func formatRupees(paise int64) string {
	sign := ""
	if paise < 0 {
		sign, paise = "-", -paise
	}
	whole := strconv.FormatInt(paise/100, 10)
	if len(whole) > 3 {
		head, tail := whole[:len(whole)-3], whole[len(whole)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		whole = strings.Join(parts, ",") + "," + tail
	}
	return fmt.Sprintf("%s₹%s.%02d", sign, whole, paise%100)
}

var intToken = regexp.MustCompile(`^-?[0-9]+$`)

// settingJSON turns what a form posted for a setting into the JSON document
// settings.Set validates. Typed controls post plain text (a number, an option,
// "24, 72"); raw JSON is still accepted. Whatever does not fit is passed on
// as it is, so the validator, not this function, refuses it.
func settingJSON(key string, posted []string) json.RawMessage {
	v := ""
	if len(posted) > 0 {
		v = strings.TrimSpace(posted[len(posted)-1]) // a checkbox posts a hidden false before its true
	}
	m, ok := settings.MetaFor(key)
	if !ok {
		return json.RawMessage(v)
	}
	switch m.Kind {
	case settings.KindEnum, settings.KindString, settings.KindClock:
		if strings.HasPrefix(v, `"`) {
			return json.RawMessage(v)
		}
		b, _ := json.Marshal(v)
		return b
	case settings.KindList:
		if strings.HasPrefix(v, "[") {
			return json.RawMessage(v)
		}
		toks := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' })
		items := make([]string, len(toks))
		for i, t := range toks {
			if intToken.MatchString(t) {
				items[i] = t
			} else {
				b, _ := json.Marshal(t)
				items[i] = string(b)
			}
		}
		return json.RawMessage("[" + strings.Join(items, ",") + "]")
	}
	return json.RawMessage(v)
}

// Labels and groups are presentation metadata; validation remains in settings.
func settingLabel(key string) string {
	labels := map[string]string{"payment_ttl": "Payment expiry", "duplicate_payment_action": "Duplicate payments", "billing_details": "Customer billing details", "pay_page_retry_buttons": "Checkout retry options", "gst_rate_bp": "GST rate", "sac_code": "SAC code", "issue_invoices": "GST invoicing", "b2cl_threshold": "B2CL reporting threshold", "mandate_afa_limit": "Mandate authentication limit", "upi_execution_windows": "UPI execution exclusions"}
	if label, ok := labels[key]; ok {
		return label
	}
	return strings.ToUpper(key[:1]) + strings.ReplaceAll(key[1:], "_", " ")
}
func settingGroup(key string) string {
	switch key {
	case "payment_ttl", "duplicate_payment_action", "billing_details", "pay_page_retry_buttons":
		return "Payment behavior"
	case "routing":
		return "Routing"
	case "dunning_schedule", "dunning_final_action", "predebit_lead", "predebit_email", "mandate_headroom", "mandate_afa_limit", "mandate_validity", "upi_execution_windows":
		return "Subscriptions and recovery"
	case "theme", "customer_language":
		return "Appearance and language"
	case "webhook_retry_schedule", "recon_time", "provider_event_retention", "document_retention", "log_retention_error_days", "log_retention_warn_days", "log_max_rows", "audit_retention_days":
		return "Delivery and retention"
	case "session_idle_hours", "session_max_days", "session_remember_days":
		return "Sign-in sessions"
	default:
		return "Invoices and tax"
	}
}
func sortSettingRows(rows []settingRow) {
	order := map[string]int{"Payment behavior": 0, "Routing": 1, "Subscriptions and recovery": 2, "Invoices and tax": 3, "Delivery and retention": 4, "Appearance and language": 5, "Sign-in sessions": 6}
	sort.SliceStable(rows, func(i, j int) bool { return order[rows[i].Group] < order[rows[j].Group] })
}
