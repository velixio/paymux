package connector

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// originRE is the only shape of source a connector may declare: an https
// origin, optionally with a leading wildcard label. Anything else (keywords,
// schemes, paths, spaces, semicolons) is dropped, so a connector cannot widen
// a policy to 'unsafe-inline' or smuggle in another directive.
var originRE = regexp.MustCompile(`^https://(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)

// ValidOrigin reports whether o is a source CheckoutHosts accepts:
// https://host[:port] or https://*.host[:port].
func ValidOrigin(o string) bool { return originRE.MatchString(o) }

// Allowed reports whether the action only sends the customer to hosts h
// declares: the script URL of an sdk action must be in h.Script, the URL of a
// form_post in h.Form and that of a redirect in h.Redirect. The URL must be
// https (a redirect to h.Self may use its scheme). A leading "*." in a listed
// origin matches one or more labels, as in a Content-Security-Policy; a
// listed entry that is not a ValidOrigin matches nothing.
func (n NextAction) Allowed(h CheckoutHosts) error {
	var raw string
	var list []string
	switch n.Kind {
	case ActionSDK:
		raw, list = n.ScriptURL, h.Script
	case ActionFormPost:
		raw, list = n.URL, h.Form
	case ActionRedirect:
		raw, list = n.URL, h.Redirect
	default:
		return fmt.Errorf("unknown next action %q", n.Kind)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%s URL %q is not an absolute URL", n.Kind, raw)
	}
	if n.Kind == ActionRedirect && h.Self != "" && u.Scheme+"://"+strings.ToLower(u.Host) == strings.ToLower(h.Self) {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s URL %q is not https", n.Kind, raw)
	}
	origin := "https://" + strings.ToLower(u.Host)
	for _, o := range list {
		if originMatches(o, origin) {
			return nil
		}
	}
	return fmt.Errorf("%s URL host %q is not among the connector's declared checkout hosts", n.Kind, u.Host)
}

// originMatches reports whether origin (https://host[:port], lower case) is
// covered by the declared source decl.
func originMatches(decl, origin string) bool {
	if !ValidOrigin(decl) {
		return false
	}
	if !strings.HasPrefix(decl, "https://*.") {
		return decl == origin
	}
	// https://*.example.com[:port]: the same port, and at least one label more.
	suffix := "." + strings.TrimPrefix(decl, "https://*.")
	return len(origin) > len("https://")+len(suffix) && strings.HasSuffix(origin, suffix)
}

// HostsOf returns the checkout hosts the hub holds c to: what the connector
// reports through CheckoutHoster (per account), else the static
// Info.CheckoutHosts. A connector with neither declares none, so every
// off-site URL it returns is refused.
func HostsOf(c Connector) CheckoutHosts {
	if h, ok := c.(CheckoutHoster); ok {
		return h.CheckoutHosts()
	}
	if h := c.Info().CheckoutHosts; h != nil {
		return *h
	}
	return CheckoutHosts{}
}
