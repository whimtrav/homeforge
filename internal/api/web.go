package api

import (
	"embed"
	"io/fs"
)

//go:embed all:webdist
var embeddedWeb embed.FS

func webFS() fs.FS {
	sub, err := fs.Sub(embeddedWeb, "webdist")
	if err != nil {
		panic(err)
	}
	return sub
}
