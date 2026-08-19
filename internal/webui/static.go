package webui

import "embed"

//go:embed static
var staticFS embed.FS

var indexHTML []byte

func init() {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		panic("webui: static/index.html missing from embed: " + err.Error())
	}
	indexHTML = b
}
