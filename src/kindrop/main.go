// kindrop - a dumb HTTP drop box for the Kindle Voyage SoftAP.
//
// Phase 2a of the plan: the phone is the device with a camera and a browser, so
// the cheapest possible receiver is an upload form served off the Kindle itself.
// No dependencies, no cgo, so it cross-compiles to a single static armv7 binary.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const page = `<!doctype html>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Kindle drop</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;padding:2rem;background:#fff;color:#111}
h1{font-size:1.25rem;margin:0 0 1rem}
input[type=file]{display:block;margin:1rem 0;width:100%%}
button{font-size:1rem;padding:.75rem 1.25rem;width:100%%}
.ok{color:#0a7a2f}
</style>
<h1>Send to Kindle</h1>
<form method=POST action=/upload enctype=multipart/form-data>
  <input type=file name=f multiple required>
  <button type=submit>Upload</button>
</form>
<p>Files land in %s</p>
`

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dest := flag.String("dest", "/mnt/us/documents", "destination directory")
	flag.Parse()

	if err := os.MkdirAll(*dest, 0o755); err != nil {
		log.Fatalf("cannot create %s: %v", *dest, err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page, *dest)
	})

	// Android probes these to decide whether a network has internet. Answering
	// keeps it from dropping the SSID or nagging about "no internet access".
	for _, p := range []string{"/generate_204", "/gen_204"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Stream rather than buffer: the Kindle has 512MB and books are big.
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var saved []string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if part.FileName() == "" {
				continue
			}
			name := safeName(part.FileName())
			out := filepath.Join(*dest, name)
			f, err := os.Create(out)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			n, err := io.Copy(f, part)
			cerr := f.Close()
			if err != nil || cerr != nil {
				os.Remove(out)
				http.Error(w, "write failed", http.StatusInternalServerError)
				return
			}
			log.Printf("saved %s (%d bytes)", out, n)
			saved = append(saved, fmt.Sprintf("%s (%d bytes)", name, n))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<meta name=viewport content=\"width=device-width,initial-scale=1\">"+
			"<p class=ok>Saved:<br>%s</p><p><a href=/>another</a></p>",
			strings.Join(saved, "<br>"))
	})

	// Log every request. Without this, "the page didn't load" is ambiguous
	// between "the browser never reached us" and "we answered and something
	// else broke" - and on this device we usually cannot watch it live.
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s from %s", r.Method, r.URL.Path, r.Proto, r.RemoteAddr)
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:         *addr,
		Handler:      logged,
		ReadTimeout:  0, // uploads over 802.11g from a phone can be slow
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("kindrop listening on %s, saving to %s", *addr, *dest)
	_, port, err := net.SplitHostPort(*addr)
	if err != nil {
		port = "8080"
	}
	for _, ip := range localIPs() {
		log.Printf("  http://%s:%s/", ip, port)
	}
	log.Fatal(srv.ListenAndServe())
}

// safeName strips any directory component a client might send, so an upload
// cannot escape the destination directory.
func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, `\`, "/"))
	if n == "" || n == "." || n == ".." || strings.HasPrefix(n, "/") {
		return fmt.Sprintf("upload-%d", time.Now().Unix())
	}
	return n
}

func localIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			out = append(out, n.IP.String())
		}
	}
	return out
}
