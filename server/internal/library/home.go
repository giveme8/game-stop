package library

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
)

//go:embed home.html
var homeHTML string

var homeTemplate = template.Must(template.New("home").Parse(homeHTML))

// Deliberately project public catalog records onto a presentation-only type:
// owner IDs, storage paths, timestamps and credentials never enter the template.
type homeCard struct {
	ID, Title, Subtitle, Description, Engine, Size, DownloadURL string
	Tags                                                        []string
}
type homePage struct {
	Games       []homeCard
	Unavailable bool
}

func packageSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	page := homePage{}
	status := http.StatusOK
	games, err := s.games(r, true) // Always public, including authenticated owners.
	if err != nil {
		page.Unavailable = true
		status = http.StatusServiceUnavailable
	} else {
		for _, game := range games {
			id, _ := game["id"].(string)
			if !idPattern.MatchString(id) {
				continue
			}
			title, _ := game["title"].(string)
			subtitle, _ := game["subtitle"].(string)
			description, _ := game["description"].(string)
			tags, _ := game["tags"].([]string)
			engine, _ := game["engine"].(Engine)
			pack, _ := game["package"].(map[string]any)
			size, _ := pack["size"].(int64)
			page.Games = append(page.Games, homeCard{ID: id, Title: title, Subtitle: subtitle, Description: description, Tags: tags, Engine: engine.Version, Size: packageSize(size), DownloadURL: "/api/v1/games/" + id + "/package.pck"})
		}
	}
	var body bytes.Buffer
	if err := homeTemplate.Execute(&body, page); err != nil {
		fail(w, http.StatusServiceUnavailable, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}
