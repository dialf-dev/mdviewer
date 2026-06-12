package main

import (
	"bytes"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
		extension.DefinitionList,
		extension.Typographer,
		highlighting.NewHighlighting(
			highlighting.WithStyle("github"),
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

var chromaCSS template.CSS

func init() {
	// Scope chroma's generated rules by the `data-theme` attribute on <html>
	// using CSS nesting, so only the active theme's rules apply.
	var buf bytes.Buffer
	f := chromahtml.New(chromahtml.WithClasses(true))

	buf.WriteString(`html[data-theme="light"] {` + "\n")
	if s := styles.Get("github"); s != nil {
		_ = f.WriteCSS(&buf, s)
	}
	buf.WriteString("}\n")

	buf.WriteString(`html[data-theme="dark"] {` + "\n")
	if s := styles.Get("github-dark"); s != nil {
		_ = f.WriteCSS(&buf, s)
	}
	buf.WriteString("}\n")

	chromaCSS = template.CSS(buf.String())
}

func renderMarkdown(path string) (template.HTML, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}
