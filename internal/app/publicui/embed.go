package publicui

import (
	"embed"
	"io/fs"
)

// GeneralsX @feature OpenAI 06/08/2026 Embed the isolated public site without mounting admin assets.
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
