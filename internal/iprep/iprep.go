// Package iprep looks up what an IP address is: its country and network, and
// whether it is a VPN, a proxy or a Tor exit. The answer is a review signal,
// never a payment decision on its own.
package iprep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Info is what a lookup said about an IP.
type Info struct {
	Country string `json:"country,omitempty"` // ISO 3166-1 alpha-2, upper case
	City    string `json:"city,omitempty"`
	Region  string `json:"region,omitempty"`
	Org     string `json:"org,omitempty"` // the network's owner, such as an ISP or a host
	VPN     bool   `json:"vpn,omitempty"`
	Proxy   bool   `json:"proxy,omitempty"`
	Tor     bool   `json:"tor,omitempty"`
	// Relay is a privacy relay such as iCloud Private Relay. Many genuine
	// buyers use one, so it does not count as masked.
	Relay bool `json:"relay,omitempty"`
}

// Masked reports whether the IP hides the buyer: a VPN, a proxy or Tor.
func (i Info) Masked() bool { return i.VPN || i.Proxy || i.Tor }

// Kind names what masks the IP ("VPN", "Proxy", "Tor"), or "" when nothing does.
func (i Info) Kind() string {
	switch {
	case i.Tor:
		return "Tor"
	case i.VPN:
		return "VPN"
	case i.Proxy:
		return "Proxy"
	}
	return ""
}

// Looker looks up one IP address.
type Looker interface {
	Lookup(ctx context.Context, ip string) (Info, error)
}

// ErrNotPublic is returned for an address no lookup service knows: private,
// loopback, link-local or unparseable.
var ErrNotPublic = errors.New("iprep: not a public IP address")

// Public reports whether ip is a public unicast address worth looking up.
func Public(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate()
}

// maxBody bounds what is read from the service.
const maxBody = 1 << 16

// VPNAPI looks IPs up at vpnapi.io. The free plan allows 1000 lookups a day.
type VPNAPI struct {
	Key     string
	BaseURL string       // default https://vpnapi.io/api/
	Client  *http.Client // default: 5 second timeout
}

// Lookup asks vpnapi.io about ip.
func (v *VPNAPI) Lookup(ctx context.Context, ip string) (Info, error) {
	if !Public(ip) {
		return Info{}, ErrNotPublic
	}
	base := v.BaseURL
	if base == "" {
		base = "https://vpnapi.io/api/"
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+url.PathEscape(ip)+"?key="+url.QueryEscape(v.Key), nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// The URL carries the key: never put it in an error that may be logged.
		return Info{}, errors.New("iprep: vpnapi.io request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Info{}, fmt.Errorf("iprep: read vpnapi.io response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("iprep: vpnapi.io answered %d", resp.StatusCode)
	}
	var out struct {
		IP       string `json:"ip"`
		Message  string `json:"message"`
		Security *struct {
			VPN   bool `json:"vpn"`
			Proxy bool `json:"proxy"`
			Tor   bool `json:"tor"`
			Relay bool `json:"relay"`
		} `json:"security"`
		Location struct {
			City        string `json:"city"`
			Region      string `json:"region"`
			CountryCode string `json:"country_code"`
		} `json:"location"`
		Network struct {
			Org string `json:"autonomous_system_organization"`
		} `json:"network"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Info{}, fmt.Errorf("iprep: decode vpnapi.io response: %w", err)
	}
	if out.Security == nil {
		// An error (a bad key, the daily limit) comes back as a message.
		return Info{}, fmt.Errorf("iprep: vpnapi.io gave no answer: %.200s", out.Message)
	}
	return Info{Country: strings.ToUpper(out.Location.CountryCode), City: out.Location.City, Region: out.Location.Region,
		Org: out.Network.Org, VPN: out.Security.VPN, Proxy: out.Security.Proxy, Tor: out.Security.Tor, Relay: out.Security.Relay}, nil
}

// EchoIP looks IPs up at an echoip server (github.com/mpolden/echoip), such as
// a self-hosted one: GET {BaseURL}/{ip}/json. It gives the country, place and
// network owner, but cannot tell a VPN, so Masked is always false.
type EchoIP struct {
	BaseURL string       // such as https://ip.example.com, no trailing slash needed
	Client  *http.Client // default: 5 second timeout
}

// Lookup asks the echoip server about ip.
func (e *EchoIP) Lookup(ctx context.Context, ip string) (Info, error) {
	if !Public(ip) {
		return Info{}, ErrNotPublic
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.BaseURL, "/")+"/"+url.PathEscape(ip)+"/json", nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, fmt.Errorf("iprep: echoip request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Info{}, fmt.Errorf("iprep: read echoip response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("iprep: echoip answered %d", resp.StatusCode)
	}
	var out struct {
		CountryISO string `json:"country_iso"`
		RegionName string `json:"region_name"`
		City       string `json:"city"`
		ASNOrg     string `json:"asn_org"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Info{}, fmt.Errorf("iprep: decode echoip response: %w", err)
	}
	if out.CountryISO == "" {
		return Info{}, errors.New("iprep: echoip does not know the IP's country")
	}
	return Info{Country: strings.ToUpper(out.CountryISO), City: out.City, Region: out.RegionName, Org: out.ASNOrg}, nil
}

// Chain asks each Looker in turn and returns the first answer, so a free
// country lookup can stand in when a VPN lookup fails or is over its limit.
type Chain []Looker

// Lookup returns the first answer of the chain, or the last error.
func (c Chain) Lookup(ctx context.Context, ip string) (Info, error) {
	err := errors.New("iprep: no lookup service")
	for _, l := range c {
		var info Info
		if info, err = l.Lookup(ctx, ip); err == nil {
			return info, nil
		}
	}
	return Info{}, err
}
