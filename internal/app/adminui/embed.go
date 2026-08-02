package adminui

import (
	"embed"
	"io/fs"
)

// content is replaced by the production frontend build before the Go binary
// is compiled. Keeping the assets embedded lets the final scratch image serve
// the admin interface without a writable or separately mounted web root.
//
//go:embed all:dist
var content embed.FS

func Files() fs.FS {
	files, err := fs.Sub(content, "dist")
	if err != nil {
		panic(err)
	}
	return files
}
