package library

import (
	_ "embed"
	"net/http"
)

//go:embed game-stop.js
var sdkJavaScript string

func (s *Server) sdkScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(sdkJavaScript))
	}
}
