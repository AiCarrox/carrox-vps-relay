// Static UI: panel is a single HTML file embedded into the binary, so we serve
// it directly without involving http.FileServer (avoids the dir-redirect quirk
// when serving "/" out of an fs.Sub).
package main

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

func registerUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
}
