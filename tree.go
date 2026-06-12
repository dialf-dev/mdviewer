package main

import (
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	svgFolder = `<svg class="tree-icon" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M1.75 2A1.75 1.75 0 0 0 0 3.75v8.5C0 13.216.784 14 1.75 14h12.5A1.75 1.75 0 0 0 16 12.25v-7A1.75 1.75 0 0 0 14.25 3.5H7.5l-.943-.943A1.75 1.75 0 0 0 5.318 2H1.75Z"/></svg>`
	svgFile   = `<svg class="tree-icon file" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M2 1.75C2 .784 2.784 0 3.75 0h6.586c.464 0 .909.184 1.237.513l2.914 2.914c.329.328.513.773.513 1.237v9.586A1.75 1.75 0 0 1 13.25 16h-9.5A1.75 1.75 0 0 1 2 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 0 0 .25-.25V6h-2.75A1.75 1.75 0 0 1 9 4.25V1.5Zm6.75.062V4.25c0 .138.112.25.25.25h2.688l-.011-.013-2.914-2.914-.013-.011Z"/></svg>`
)

type fileNode struct {
	Name     string
	Abs      string
	IsDir    bool
	Children []*fileNode
}

func walkTree(abs string) *fileNode {
	node := &fileNode{Name: filepath.Base(abs), Abs: abs, IsDir: true}
	fillNode(node)
	return node
}

func fillNode(n *fileNode) {
	entries, err := os.ReadDir(n.Abs)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		childAbs := filepath.Join(n.Abs, name)
		if e.IsDir() {
			c := &fileNode{Name: name, Abs: childAbs, IsDir: true}
			fillNode(c)
			if treeHasMarkdown(c) {
				n.Children = append(n.Children, c)
			}
		} else if isMarkdown(name) {
			n.Children = append(n.Children, &fileNode{Name: name, Abs: childAbs})
		}
	}
	sort.Slice(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

func treeHasMarkdown(n *fileNode) bool {
	for _, c := range n.Children {
		if !c.IsDir {
			return true
		}
		if treeHasMarkdown(c) {
			return true
		}
	}
	return false
}

// renderSidebar builds the full sidebar HTML: registered roots (parent/child
// merged) and temp roots in a separate section. File links use opaque doc
// IDs so filesystem paths never appear in URLs.
func (s *server) renderSidebar(current string) template.HTML {
	regs, temps := s.visibleRoots()
	var b strings.Builder
	if len(regs) == 0 && len(temps) == 0 {
		b.WriteString(`<div class="filelist-empty">No registered directories</div>`)
		return template.HTML(b.String())
	}
	if len(regs) > 0 {
		b.WriteString(`<div class="tree-section">Directories</div><ul class="filetree filetree-root">`)
		for _, r := range regs {
			s.writeRoot(&b, r, current, false)
		}
		b.WriteString(`</ul>`)
	}
	if len(temps) > 0 {
		b.WriteString(`<div class="tree-section">Temp</div><ul class="filetree filetree-root">`)
		for _, r := range temps {
			s.writeRoot(&b, r, current, true)
		}
		b.WriteString(`</ul>`)
	}
	return template.HTML(b.String())
}

func (s *server) writeRoot(b *strings.Builder, root, current string, temp bool) {
	node := walkTree(root)
	open := current == root || strings.HasPrefix(current, root+string(filepath.Separator))
	b.WriteString(`<li><details class="tree-dir tree-root`)
	if temp {
		b.WriteString(" tree-root-temp")
	}
	b.WriteString(`"`)
	if open {
		b.WriteString(" open")
	}
	b.WriteString(`><summary title="`)
	template.HTMLEscape(b, []byte(root))
	b.WriteString(`"><span class="tree-caret" aria-hidden="true"></span>`)
	b.WriteString(svgFolder)
	b.WriteString(`<span class="tree-label">`)
	template.HTMLEscape(b, []byte(node.Name))
	b.WriteString(`</span>`)
	if temp {
		b.WriteString(`<span class="tree-badge">temp</span>`)
	}
	b.WriteString(`</summary>`)
	s.writeChildren(b, node.Children, current)
	b.WriteString(`</details></li>`)
}

func (s *server) writeChildren(b *strings.Builder, nodes []*fileNode, current string) {
	b.WriteString(`<ul class="filetree">`)
	for _, n := range nodes {
		b.WriteString("<li>")
		if n.IsDir {
			open := strings.HasPrefix(current, n.Abs+string(filepath.Separator))
			b.WriteString(`<details class="tree-dir"`)
			if open {
				b.WriteString(" open")
			}
			b.WriteString(`><summary><span class="tree-caret" aria-hidden="true"></span>`)
			b.WriteString(svgFolder)
			b.WriteString(`<span class="tree-label">`)
			template.HTMLEscape(b, []byte(n.Name))
			b.WriteString(`</span></summary>`)
			s.writeChildren(b, n.Children, current)
			b.WriteString(`</details>`)
		} else {
			id := s.rememberDoc(n.Abs)
			b.WriteString(`<a class="filelist-item`)
			if n.Abs == current {
				b.WriteString(" active")
			}
			b.WriteString(`" href="/d/`)
			b.WriteString(id)
			b.WriteString(`" title="`)
			template.HTMLEscape(b, []byte(n.Name))
			b.WriteString(`">`)
			b.WriteString(svgFile)
			b.WriteString(`<span class="tree-label">`)
			template.HTMLEscape(b, []byte(n.Name))
			b.WriteString(`</span></a>`)
		}
		b.WriteString("</li>")
	}
	b.WriteString(`</ul>`)
}
