package main

import (
	"embed"
	"html/template"
)

//go:embed web/view/*.html
var webFS embed.FS

var webTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"sub":      func(a, b int) int { return a - b },
	"jsAsset":  jsAsset,
	"cssAsset": cssAsset,
}).ParseFS(webFS, "web/view/*.html"))
