package library

import (
	"bytes"
	_ "embed"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

//go:embed connect.html
var connectHTML string

//go:embed connect.js
var connectJS string
var connectTemplate = template.Must(template.New("connect").Parse(connectHTML))
var connectStatePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var originHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

// Accept only serialized origins, never URLs with paths, credentials or a
// browser-dependent spelling (backslashes, shorthand IPs, default ports).
func validConnectOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\\#%") || u.Opaque != "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip != nil && ip.String() != host {
		return false
	}
	if ip == nil {
		labels := strings.Split(host, ".")
		for _, label := range labels {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return false
			}
		}
		last := labels[len(labels)-1]
		if _, err := strconv.ParseUint(last, 0, 64); err == nil {
			return false
		}
		if _, err := strconv.ParseUint(last, 10, 64); err == nil {
			return false
		}
	}
	if !originHostPattern.MatchString(host) && ip == nil {
		return false
	}
	if strings.Contains(host, ":") && (ip == nil || !strings.HasPrefix(u.Host, "[")) {
		return false
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
			return false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return host == "localhost" || (ip != nil && ip.To4() != nil && ip.String() == host && (ip.IsPrivate() || ip.IsLoopback()))
}

func connectHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Opener-Policy", "unsafe-none")
}

func (s *Server) connectPage(w http.ResponseWriter, r *http.Request) {
	connectHeaders(w)
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, 400, "invalid connect parameters")
		return
	}
	for key, values := range q {
		if len(values) != 1 || (key != "gameId" && key != "mode" && key != "origin" && key != "state") {
			fail(w, 400, "invalid connect parameters")
			return
		}
	}
	game, mode, origin, state := q.Get("gameId"), normalizedMode(q.Get("mode")), q.Get("origin"), q.Get("state")
	if !idPattern.MatchString(game) || !idPattern.MatchString(mode) || !validConnectOrigin(origin) || !connectStatePattern.MatchString(state) {
		fail(w, 400, "invalid connect parameters")
		return
	}
	var body bytes.Buffer
	if err := connectTemplate.Execute(&body, struct {
		GameID, Mode, Origin, State string
		LAN                         bool
	}{game, mode, origin, state, strings.HasPrefix(origin, "http:")}); err != nil {
		fail(w, 503, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func (s *Server) connectScript(w http.ResponseWriter, r *http.Request) {
	connectHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(connectJS))
	}
}
