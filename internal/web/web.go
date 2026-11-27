package web

import (
	"embed"
	"html/template"
	"net/http"

	"browsercompute.local/runtime/internal/identity"
)

//go:embed *.html *.css *.js
var assets embed.FS

var pages = template.Must(template.ParseFS(assets, "*.html"))

func Page(name string, display identity.Display) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = pages.ExecuteTemplate(w, name, display)
	}
}

func Assets() http.Handler { return http.FileServerFS(assets) }
