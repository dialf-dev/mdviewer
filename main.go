package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const usageText = `usage:
  mdv <file.md>         open a markdown file in the running viewer service
  mdv serve [--port N]  run the viewer service (normally started by systemd)
  mdv on                enable external access (bind 0.0.0.0)
  mdv off               disable external access (bind localhost only)
  mdv add [dir]         register a directory (default: current directory)
  mdv del [dir]         unregister a directory (default: current directory)
  mdv list              show registered directories
  mdv status            show service status
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		runServe(args[1:])
	case "on":
		cmdOnOff(true)
	case "off":
		cmdOnOff(false)
	case "add", "del":
		cmdAddDel(args[0], args[1:])
	case "list":
		cmdList()
	case "status":
		cmdStatus()
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		cmdTemp(args[0])
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func ctrl(req ctrlRequest) *ctrlResponse {
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot connect to the mdv service (%v)\n", err)
		fmt.Fprintln(os.Stderr, "start it with: sudo systemctl start mdv")
		os.Exit(1)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fail("send request: %v", err)
	}
	var resp ctrlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fail("read response: %v", err)
	}
	return &resp
}

func mustOK(resp *ctrlResponse) *ctrlResponse {
	if !resp.OK {
		fail("%s", resp.Msg)
	}
	return resp
}

func cmdOnOff(on bool) {
	cmd := "off"
	if on {
		cmd = "on"
	}
	resp := mustOK(ctrl(ctrlRequest{Cmd: cmd}))
	if on {
		fmt.Println("external access enabled (0.0.0.0)")
		fmt.Println("warning: anyone on the network can now read registered documents and upload files")
	} else {
		fmt.Println("external access disabled (localhost only)")
	}
	for _, u := range resp.URLs {
		fmt.Printf("  → %s\n", u)
	}
}

func cmdAddDel(cmd string, rest []string) {
	dir := ""
	if len(rest) > 0 {
		dir = rest[0]
	} else {
		wd, err := os.Getwd()
		if err != nil {
			fail("getwd: %v", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fail("abs: %v", err)
	}
	resp := mustOK(ctrl(ctrlRequest{Cmd: cmd, Path: abs}))
	if resp.Msg != "" {
		fmt.Println(resp.Msg)
	}
	printRoots(resp)
}

func cmdList() {
	printRoots(mustOK(ctrl(ctrlRequest{Cmd: "list"})))
}

func cmdStatus() {
	resp := mustOK(ctrl(ctrlRequest{Cmd: "status"}))
	mode := "localhost only (external off)"
	if resp.External {
		mode = "external (0.0.0.0)"
	}
	fmt.Println("mdv service")
	fmt.Printf("  bind:  %s\n", mode)
	fmt.Printf("  port:  %d\n", resp.Port)
	for _, u := range resp.URLs {
		fmt.Printf("  url:   %s\n", u)
	}
	printRoots(resp)
}

func printRoots(resp *ctrlResponse) {
	fmt.Println("  registered:")
	if len(resp.Roots) == 0 {
		fmt.Println("    (none)")
	}
	for _, r := range resp.Roots {
		fmt.Printf("    %s\n", r)
	}
	if len(resp.Temps) > 0 {
		fmt.Println("  temp:")
		for _, t := range resp.Temps {
			fmt.Printf("    %s\n", t)
		}
	}
}

func cmdTemp(path string) {
	if !isMarkdown(path) {
		fmt.Fprintf(os.Stderr, "error: unknown command or not a .md file: %s\n\n", path)
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		if err == nil {
			err = fmt.Errorf("is a directory")
		}
		fail("cannot open %s: %v", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fail("abs: %v", err)
	}
	resp := mustOK(ctrl(ctrlRequest{Cmd: "temp", Path: abs}))
	fmt.Printf("\n  mdv — %s\n", filepath.Base(abs))
	fmt.Printf("  ─────────────────────────────\n")
	for _, u := range resp.DocURLs {
		fmt.Printf("  → %s\n", u)
	}
	fmt.Printf("\n  (served by the mdv service; the page auto-reloads on file change)\n\n")
}
