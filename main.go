package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed assets/style.css
var assetsFS embed.FS

//go:embed templates/view.html
var viewTmplSrc string

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
	// using CSS nesting. A block like `html[data-theme="light"] { .chroma { ... } }`
	// is rewritten by the browser to `html[data-theme="light"] .chroma { ... }`,
	// so only the active theme's rules apply.
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

func listMarkdownFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isMarkdown(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out, nil
}

func resolveTarget(baseDir, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty file")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute path not allowed")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes base")
	}
	candidate, err := filepath.Abs(filepath.Join(baseDir, cleaned))
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}
	if candidate != baseDir && !strings.HasPrefix(candidate, baseDir+string(filepath.Separator)) {
		return "", errors.New("path escapes base")
	}
	if !isMarkdown(candidate) {
		return "", errors.New("not a markdown file")
	}
	st, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if st.IsDir() {
		return "", errors.New("is a directory")
	}
	return candidate, nil
}

type currentFile struct {
	mu   sync.RWMutex
	path string
}

func (c *currentFile) get() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

func (c *currentFile) set(p string) {
	c.mu.Lock()
	c.path = p
	c.mu.Unlock()
}

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan struct{}]bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan struct{}]bool)}
}

func (b *broadcaster) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *broadcaster) publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func watchDir(dir string, current *currentFile, b *broadcaster) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("new watcher: %w", err)
	}
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	go func() {
		var timer *time.Timer
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				evAbs, _ := filepath.Abs(ev.Name)
				if evAbs != current.get() {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(80*time.Millisecond, b.publish)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("watch error: %v", err)
			}
		}
	}()
	return nil
}

func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func pickListener(preferred int) (net.Listener, int, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", preferred)); err == nil {
		return ln, preferred, nil
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func main() {
	port := flag.Int("port", 8080, "preferred port (falls back to a random free port if in use)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mdv [--port N] <file.md>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		if err == nil {
			err = fmt.Errorf("is a directory")
		}
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		log.Fatalf("abs: %v", err)
	}
	baseDir := filepath.Dir(absPath)

	current := &currentFile{path: absPath}
	bc := newBroadcaster()
	if err := watchDir(baseDir, current, bc); err != nil {
		log.Fatalf("watcher: %v", err)
	}

	tmpl, err := template.New("view").Parse(viewTmplSrc)
	if err != nil {
		log.Fatalf("template: %v", err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	staticFS, _ := fs.Sub(assetsFS, "assets")
	r.StaticFS("/assets", http.FS(staticFS))

	r.GET("/", func(c *gin.Context) {
		target := absPath
		if rel := c.Query("file"); rel != "" {
			resolved, err := resolveTarget(baseDir, rel)
			if err != nil {
				c.String(http.StatusBadRequest, "invalid file: %v", err)
				return
			}
			target = resolved
		}
		current.set(target)

		body, err := renderMarkdown(target)
		if err != nil {
			c.String(http.StatusInternalServerError, "render error: %v", err)
			return
		}
		files, err := listMarkdownFiles(baseDir)
		if err != nil {
			log.Printf("list dir: %v", err)
			files = []string{filepath.Base(target)}
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusOK)
		_ = tmpl.Execute(c.Writer, map[string]any{
			"Title":     filepath.Base(target),
			"Body":      body,
			"ChromaCSS": chromaCSS,
			"Files":     files,
			"Current":   filepath.Base(target),
		})
	})

	r.GET("/events", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")

		ch := bc.subscribe()
		defer bc.unsubscribe(ch)

		fmt.Fprint(c.Writer, ": ok\n\n")
		c.Writer.Flush()

		ctx := c.Request.Context()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ch:
				fmt.Fprint(c.Writer, "event: reload\ndata: 1\n\n")
				c.Writer.Flush()
			case <-ping.C:
				fmt.Fprint(c.Writer, ": ping\n\n")
				c.Writer.Flush()
			case <-ctx.Done():
				return
			}
		}
	})

	ln, actualPort, err := pickListener(*port)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	ip := outboundIP()
	fmt.Printf("\n  mdv — %s\n", filepath.Base(absPath))
	fmt.Printf("  ─────────────────────────────\n")
	fmt.Printf("  → http://%s:%d\n", ip, actualPort)
	fmt.Printf("  → http://localhost:%d\n\n", actualPort)
	fmt.Printf("  (Ctrl+C to quit; the page auto-reloads on file change)\n\n")

	srv := &http.Server{Handler: r}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
