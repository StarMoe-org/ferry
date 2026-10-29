package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var rawDist embed.FS

// Dist returns the embedded SPA filesystem rooted at web/dist.
func Dist() (fs.FS, error) {
	return fs.Sub(rawDist, "dist")
}
