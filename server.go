package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed assets/style.css
var assetsFS embed.FS

//go:embed templates/view.html
var viewTmplSrc string

//go:embed templates/upload.html
var uploadTmplSrc string

const welcomeHTML = `<h1>mdv</h1>
<p>Select a document from the sidebar, or <a href="/upload">upload</a> markdown files.</p>
<p>Register directories from a shell with <code>mdv add</code>, or open a single file with <code>mdv path/to/file.md</code>.</p>`

type server struct {
	mu         sync.Mutex // guards cfg, tempRoots, lastTempID, actualPort, httpSrv
	cfg        *Config
	tempRoots  []string
	lastTempID string
	actualPort int
	httpSrv    *http.Server

	secret []byte
	docsMu sync.RWMutex
	docs   map[string]string // docID -> absolute path

	hub        *hub
	viewTmpl   *template.Template
	uploadTmpl *template.Template
}

func runServe(args []string) {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fset.Int("port", 0, "override configured port (not persisted)")
	_ = fset.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if err := os.MkdirAll(cfg.UploadDir, 0o755); err != nil {
		log.Printf("upload dir %s: %v", cfg.UploadDir, err)
	}

	secret, err := hex.DecodeString(cfg.Secret)
	if err != nil || len(secret) == 0 {
		log.Fatalf("config: invalid secret")
	}

	s := &server{
		cfg:    cfg,
		secret: secret,
		docs:   make(map[string]string),
	}
	s.viewTmpl = template.Must(template.New("view").Parse(viewTmplSrc))
	s.uploadTmpl = template.Must(template.New("upload").Parse(uploadTmplSrc))

	h, err := newHub(s.docID)
	if err != nil {
		log.Fatalf("watcher: %v", err)
	}
	s.hub = h

	s.rebuildDocs()

	if err := s.serveControl(); err != nil {
		log.Fatalf("control socket: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = os.Remove(socketPath())
		os.Exit(0)
	}()

	if err := s.run(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

/* ---------- doc ID mapping ---------- */

func (s *server) docID(path string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(path))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

func (s *server) rememberDoc(path string) string {
	id := s.docID(path)
	s.docsMu.Lock()
	s.docs[id] = path
	s.docsMu.Unlock()
	return id
}

func (s *server) lookupDoc(id string) (string, bool) {
	s.docsMu.RLock()
	defer s.docsMu.RUnlock()
	p, ok := s.docs[id]
	return p, ok
}

func (s *server) rebuildDocs() {
	for _, r := range s.allowedRoots() {
		s.indexRoot(r)
	}
}

func (s *server) indexRoot(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if isMarkdown(d.Name()) {
			s.rememberDoc(p)
		}
		return nil
	})
}

/* ---------- root management ---------- */

func underOrEqual(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

func strictlyUnder(root, p string) bool {
	return p != root && strings.HasPrefix(p, root+string(filepath.Separator))
}

func (s *server) allowedRoots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.cfg.Roots)+len(s.tempRoots)+1)
	out = append(out, s.cfg.Roots...)
	out = append(out, s.cfg.UploadDir)
	out = append(out, s.tempRoots...)
	return out
}

func (s *server) isAllowedFile(path string) bool {
	if !isMarkdown(path) {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	for _, r := range s.allowedRoots() {
		if strictlyUnder(r, path) {
			return true
		}
	}
	return false
}

// visibleRoots returns the sidebar's top-level roots: registered roots with
// children of other roots merged away, then temp roots that are not already
// covered by a registered root.
func (s *server) visibleRoots() (regs, temps []string) {
	s.mu.Lock()
	allReg := append([]string{}, s.cfg.Roots...)
	allReg = append(allReg, s.cfg.UploadDir)
	allTemp := append([]string{}, s.tempRoots...)
	s.mu.Unlock()

	regs = mergeRoots(allReg)
	for _, t := range mergeRoots(allTemp) {
		covered := false
		for _, r := range regs {
			if underOrEqual(r, t) {
				covered = true
				break
			}
		}
		if !covered {
			temps = append(temps, t)
		}
	}
	return regs, temps
}

// mergeRoots dedupes exact entries and drops any root that lies under
// another root in the same set.
func mergeRoots(in []string) []string {
	seen := make(map[string]bool)
	var uniq []string
	for _, r := range in {
		if !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	var out []string
	for _, r := range uniq {
		child := false
		for _, o := range uniq {
			if o != r && strictlyUnder(o, r) {
				child = true
				break
			}
		}
		if !child {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

/* ---------- HTTP ---------- */

func (s *server) router() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	staticFS, _ := fs.Sub(assetsFS, "assets")
	r.StaticFS("/assets", http.FS(staticFS))

	r.GET("/", s.handleHome)
	r.GET("/d/:id", s.handleDoc)
	r.GET("/events", s.handleEvents)
	r.GET("/upload", s.handleUploadPage)
	r.POST("/api/upload", s.handleUploadPost)
	return r
}

func (s *server) renderView(c *gin.Context, docID, currentPath, title string, body template.HTML) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	_ = s.viewTmpl.Execute(c.Writer, map[string]any{
		"Title":     title,
		"Body":      body,
		"ChromaCSS": chromaCSS,
		"Tree":      s.renderSidebar(currentPath),
		"DocID":     docID,
	})
}

func (s *server) handleHome(c *gin.Context) {
	s.mu.Lock()
	last := s.lastTempID
	s.mu.Unlock()
	if last != "" {
		if p, ok := s.lookupDoc(last); ok && s.isAllowedFile(p) {
			c.Redirect(http.StatusFound, "/d/"+last)
			return
		}
	}
	s.renderView(c, "", "", "mdv", template.HTML(welcomeHTML))
}

func (s *server) handleDoc(c *gin.Context) {
	id := c.Param("id")
	path, ok := s.lookupDoc(id)
	if !ok || !s.isAllowedFile(path) {
		c.String(http.StatusNotFound, "document not found")
		return
	}
	body, err := renderMarkdown(path)
	if err != nil {
		c.String(http.StatusInternalServerError, "render error: %v", err)
		return
	}
	s.renderView(c, id, path, filepath.Base(path), body)
}

func (s *server) handleEvents(c *gin.Context) {
	id := c.Query("d")
	path, ok := s.lookupDoc(id)
	if !ok || !s.isAllowedFile(path) {
		c.String(http.StatusNotFound, "unknown document")
		return
	}
	dir := filepath.Dir(path)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ch := s.hub.subscribe(id, dir)
	defer s.hub.unsubscribe(id, dir, ch)

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
}

/* ---------- upload ---------- */

func (s *server) handleUploadPage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusOK)
	_ = s.uploadTmpl.Execute(c.Writer, map[string]any{
		"Title": "Upload",
	})
}

func (s *server) handleUploadPost(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "no files"})
		return
	}
	s.mu.Lock()
	uploadDir := s.cfg.UploadDir
	s.mu.Unlock()
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}

	type result struct {
		Name  string `json:"name"`
		URL   string `json:"url,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var results []result
	for _, fh := range files {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		switch {
		case name == "" || name == "." || strings.HasPrefix(name, "."):
			results = append(results, result{Name: fh.Filename, Error: "invalid file name"})
			continue
		case !isMarkdown(name):
			results = append(results, result{Name: name, Error: "only .md / .markdown files are allowed"})
			continue
		case fh.Size > maxUploadSize:
			results = append(results, result{Name: name, Error: "file too large (max 20MB)"})
			continue
		}
		dest := uniquePath(uploadDir, name)
		if err := saveMultipart(fh, dest); err != nil {
			results = append(results, result{Name: name, Error: err.Error()})
			continue
		}
		id := s.rememberDoc(dest)
		results = append(results, result{Name: filepath.Base(dest), URL: "/d/" + id})
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "files": results})
}

func saveMultipart(fh *multipart.FileHeader, dest string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	cand := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
		cand = filepath.Join(dir, fmt.Sprintf("%s(%d)%s", stem, i, ext))
	}
}

/* ---------- control socket ---------- */

type ctrlRequest struct {
	Cmd  string `json:"cmd"`
	Path string `json:"path,omitempty"`
}

type ctrlResponse struct {
	OK       bool     `json:"ok"`
	Msg      string   `json:"msg,omitempty"`
	External bool     `json:"external"`
	Port     int      `json:"port,omitempty"`
	URLs     []string `json:"urls,omitempty"`
	Roots    []string `json:"roots,omitempty"`
	Temps    []string `json:"temps,omitempty"`
	DocURLs  []string `json:"doc_urls,omitempty"`
}

func (s *server) serveControl() error {
	sp := socketPath()
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}
	_ = os.Remove(sp)
	ln, err := net.Listen("unix", sp)
	if err != nil {
		return err
	}
	// No auth by design (decided spec); any local user may control the
	// service, matching the open-access posture.
	_ = os.Chmod(sp, 0o666)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleCtrlConn(conn)
		}
	}()
	return nil
}

func (s *server) handleCtrlConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req ctrlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := s.handleCtrl(req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (s *server) handleCtrl(req ctrlRequest) ctrlResponse {
	switch req.Cmd {
	case "status", "list":
		return s.statusResp("")
	case "on":
		return s.setExternal(true)
	case "off":
		return s.setExternal(false)
	case "add":
		return s.addRoot(req.Path)
	case "del":
		return s.delRoot(req.Path)
	case "temp":
		return s.openTemp(req.Path)
	}
	return ctrlResponse{OK: false, Msg: "unknown command: " + req.Cmd}
}

func (s *server) statusResp(msg string) ctrlResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	port := s.actualPort
	if port == 0 {
		port = s.cfg.Port
	}
	urls := []string{fmt.Sprintf("http://localhost:%d", port)}
	if s.cfg.External {
		urls = append([]string{fmt.Sprintf("http://%s:%d", outboundIP(), port)}, urls...)
	}
	return ctrlResponse{
		OK:       true,
		Msg:      msg,
		External: s.cfg.External,
		Port:     port,
		URLs:     urls,
		Roots:    append([]string{}, s.cfg.Roots...),
		Temps:    append([]string{}, s.tempRoots...),
	}
}

func (s *server) setExternal(on bool) ctrlResponse {
	s.mu.Lock()
	changed := s.cfg.External != on
	s.cfg.External = on
	if err := saveConfig(s.cfg); err != nil {
		s.mu.Unlock()
		return ctrlResponse{OK: false, Msg: "save config: " + err.Error()}
	}
	srv := s.httpSrv
	s.mu.Unlock()
	if changed && srv != nil {
		_ = srv.Close() // run() rebinds with the new address
	}
	return s.statusResp("")
}

func (s *server) addRoot(path string) ctrlResponse {
	if path == "" || !filepath.IsAbs(path) {
		return ctrlResponse{OK: false, Msg: "absolute path required"}
	}
	clean := filepath.Clean(path)
	st, err := os.Stat(clean)
	if err != nil || !st.IsDir() {
		return ctrlResponse{OK: false, Msg: "not a directory: " + clean}
	}
	s.mu.Lock()
	for _, r := range s.cfg.Roots {
		if r == clean {
			s.mu.Unlock()
			return s.statusResp("already registered: " + clean)
		}
	}
	s.cfg.Roots = append(s.cfg.Roots, clean)
	if err := saveConfig(s.cfg); err != nil {
		s.cfg.Roots = s.cfg.Roots[:len(s.cfg.Roots)-1]
		s.mu.Unlock()
		return ctrlResponse{OK: false, Msg: "save config: " + err.Error()}
	}
	s.mu.Unlock()
	s.indexRoot(clean)
	return s.statusResp("registered: " + clean)
}

func (s *server) delRoot(path string) ctrlResponse {
	if path == "" || !filepath.IsAbs(path) {
		return ctrlResponse{OK: false, Msg: "absolute path required"}
	}
	clean := filepath.Clean(path)
	s.mu.Lock()
	idx := -1
	for i, r := range s.cfg.Roots {
		if r == clean {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return ctrlResponse{OK: false, Msg: "not registered: " + clean}
	}
	s.cfg.Roots = append(s.cfg.Roots[:idx], s.cfg.Roots[idx+1:]...)
	if err := saveConfig(s.cfg); err != nil {
		s.mu.Unlock()
		return ctrlResponse{OK: false, Msg: "save config: " + err.Error()}
	}
	s.mu.Unlock()
	return s.statusResp("unregistered: " + clean)
}

func (s *server) openTemp(path string) ctrlResponse {
	if path == "" || !filepath.IsAbs(path) {
		return ctrlResponse{OK: false, Msg: "absolute path required"}
	}
	clean := filepath.Clean(path)
	if !isMarkdown(clean) {
		return ctrlResponse{OK: false, Msg: "not a markdown file: " + clean}
	}
	st, err := os.Stat(clean)
	if err != nil || st.IsDir() {
		return ctrlResponse{OK: false, Msg: "cannot open: " + clean}
	}
	dir := filepath.Dir(clean)
	s.mu.Lock()
	found := false
	for _, t := range s.tempRoots {
		if t == dir {
			found = true
			break
		}
	}
	if !found {
		s.tempRoots = append(s.tempRoots, dir)
	}
	s.mu.Unlock()
	s.indexRoot(dir)
	id := s.rememberDoc(clean)
	s.mu.Lock()
	s.lastTempID = id
	s.mu.Unlock()

	resp := s.statusResp("")
	for _, u := range resp.URLs {
		resp.DocURLs = append(resp.DocURLs, u+"/d/"+id)
	}
	return resp
}

/* ---------- listener ---------- */

func (s *server) run() error {
	handler := s.router()
	for {
		s.mu.Lock()
		bind := "127.0.0.1"
		if s.cfg.External {
			bind = "0.0.0.0"
		}
		port := s.cfg.Port
		s.mu.Unlock()

		ln, actual, err := pickListener(bind, port)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: handler}
		s.mu.Lock()
		s.actualPort = actual
		s.httpSrv = srv
		s.mu.Unlock()

		mode := "localhost only"
		if bind == "0.0.0.0" {
			mode = "external"
		}
		log.Printf("mdv listening on %s:%d (%s)", bind, actual, mode)

		err = srv.Serve(ln)
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		// Closed by setExternal for a rebind; loop with the new address.
	}
}

func pickListener(bind string, preferred int) (net.Listener, int, error) {
	if ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, preferred)); err == nil {
		return ln, preferred, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:0", bind))
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
