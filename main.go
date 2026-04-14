package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

func watchFile(path string, b *broadcaster) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		return err
	}
	abs, _ := filepath.Abs(path)
	go func() {
		var timer *time.Timer
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				evAbs, _ := filepath.Abs(ev.Name)
				if evAbs != abs {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
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
	absPath, _ := filepath.Abs(path)

	bc := newBroadcaster()
	if err := watchFile(absPath, bc); err != nil {
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
		body, err := renderMarkdown(absPath)
		if err != nil {
			c.String(http.StatusInternalServerError, "render error: %v", err)
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusOK)
		_ = tmpl.Execute(c.Writer, map[string]any{
			"Title":     filepath.Base(absPath),
			"Body":      body,
			"ChromaCSS": chromaCSS,
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
