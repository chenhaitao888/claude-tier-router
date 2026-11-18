package router

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var dashboardHTML []byte

func (g *Gateway) dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Write(dashboardHTML)
}
