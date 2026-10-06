// Package web serves the dashboard and its bundled browser dependencies.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded dashboard without requiring files on disk.
func Handler() http.Handler {
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The embed directive guarantees that this directory exists.
		panic(err)
	}
	return http.FileServerFS(assets)
}
