// Package webassets serves the dashboard and its locally vendored dependencies.
package webassets

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var assets embed.FS

// Handler serves embedded files, with index.html at / and 404s for unknown paths.
func Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		// The directory is guaranteed to exist by the embed directive.
		panic(err)
	}
	return http.FileServerFS(static)
}
