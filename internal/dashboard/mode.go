package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	cookieMode  = "pm_mode"
	defaultMode = "live"
)

func (d *Dashboard) modeSig(sessionID, mode string) string {
	m := hmac.New(sha256.New, d.modeKey)
	m.Write([]byte(sessionID + "|" + mode))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// mode is the test/live mode the session views. The choice lives in a cookie
// signed for this session id, so it neither survives a new login nor can be
// forged or moved to another session.
func (d *Dashboard) mode(r *http.Request, s *session) string {
	c, err := r.Cookie(cookieMode)
	if err != nil {
		return defaultMode
	}
	mode, sig, ok := strings.Cut(c.Value, ".")
	if !ok || (mode != "test" && mode != "live") ||
		!hmac.Equal([]byte(sig), []byte(d.modeSig(s.ID, mode))) {
		return defaultMode
	}
	return mode
}

// safeNext returns a same-site path to redirect back to, or fallback.
func safeNext(next, fallback string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') {
		return fallback
	}
	// http.Redirect cleans the path, so check the cleaned one: /dashboard/../x
	// must not leave the dashboard.
	clean := path.Clean(u.Path)
	if !strings.HasPrefix(clean, prefix+"/") {
		return fallback
	}
	u.Path, u.RawPath = clean, ""
	return u.String()
}

func (d *Dashboard) setMode(w http.ResponseWriter, r *http.Request, s *session) {
	mode := r.PostForm.Get("mode")
	if mode != "test" && mode != "live" {
		http.Error(w, "mode must be test or live", http.StatusBadRequest)
		return
	}
	d.setCookie(w, cookieMode, mode+"."+d.modeSig(s.ID, mode), 0)
	http.Redirect(w, r, safeNext(r.PostForm.Get("next"), prefix+"/overview"), http.StatusSeeOther)
}
