// directory serves a LAN-only page listing every route in ../routes.yml:
// the public hostname, the machine it runs on and the local port, each as a
// link. routes.yml is read on every request, so the page is never stale after
// ./routes or git-sync changes it. Caddy exposes it on 192.168.1.1:81 to the
// LAN only (see ../Caddyfile); this process listens on loopback.
//
// The page also asks for DIRECTORY_PASSWORD (from the env file, via ./serve)
// once per browser: a correct password sets a one-year cookie holding a hash
// of it, so changing the password logs every browser out.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// machines maps a tunnel name in routes.yml to the machine that runs it
// (see the my-machines rule). A tunnel missing here is listed without LAN links.
var machines = map[string]machine{
	"macmini-production":     {"macmini", "192.168.1.1", true},
	"macbookpro-development": {"immunemac", "192.168.1.2", false},
	"pc":                     {"PC", "192.168.1.3", false},
}

// LANDoors: the machine's Caddy also listens on IP:PORT for apps bound to
// 127.0.0.1 (the LAN doors block in ../Caddyfile), so their LAN URL works.
type machine struct {
	Name, IP string
	LANDoors bool
}

//go:embed page.html login.html
var pageFS embed.FS

var (
	page      = template.Must(template.ParseFS(pageFS, "page.html"))
	loginPage = template.Must(template.ParseFS(pageFS, "login.html"))
)

const cookieName = "directory_auth"

// token is what the cookie holds: never the password itself.
func token(password string) string {
	sum := sha256.Sum256([]byte("directory:" + password))
	return hex.EncodeToString(sum[:])
}

func authorized(r *http.Request, want string) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

func showLogin(w http.ResponseWriter, status int, wrong bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	loginPage.Execute(w, map[string]any{"Wrong": wrong})
}

// route is one routes.yml entry: either a bare service URL or a mapping.
type route struct {
	Service string `yaml:"service"`
	Caddy   string `yaml:"caddy"`
	Path    string `yaml:"path"`
	Note    string `yaml:"note"`
}

func (r *route) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Service = n.Value
		return nil
	}
	type plain route
	return n.Decode((*plain)(r))
}

type registry struct {
	Tunnels map[string]struct {
		ID     string           `yaml:"id"`
		Routes map[string]route `yaml:"routes"`
	} `yaml:"tunnels"`
}

type row struct {
	Host     string
	Scheme   string // http, https, ssh, ...
	Port     string
	Loopback bool   // bound to 127.0.0.1 explicitly: unreachable from the LAN
	LAN      string // http://<machine ip>:<port>, when it can be a link
	Caddy    string // the upstream port behind a generated Caddy block
	Path     string
	Note     string
}

type group struct {
	Tunnel  string
	Machine machine
	Rows    []row
}

func load(path string) ([]group, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var groups []group
	for name, t := range reg.Tunnels {
		g := group{Tunnel: name, Machine: machines[name]}
		for host, r := range t.Routes {
			g.Rows = append(g.Rows, newRow(host, r, g.Machine))
		}
		sort.Slice(g.Rows, func(i, j int) bool { return g.Rows[i].Host < g.Rows[j].Host })
		groups = append(groups, g)
	}
	// The always-on macmini first, then the rest by name.
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i].Machine.IP == "192.168.1.1", groups[j].Machine.IP == "192.168.1.1"
		if a != b {
			return a
		}
		return groups[i].Tunnel < groups[j].Tunnel
	})
	return groups, nil
}

func newRow(host string, r route, m machine) row {
	out := row{Host: host, Path: r.Path, Note: r.Note}
	u, err := url.Parse(r.Service)
	if err != nil || u.Host == "" {
		out.Scheme = r.Service // e.g. http_status:404
		return out
	}
	out.Scheme = u.Scheme
	out.Port = u.Port()
	out.Loopback = (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") && !m.LANDoors
	if out.Port != "" && m.IP != "" && (u.Scheme == "http" || u.Scheme == "https") {
		out.LAN = fmt.Sprintf("%s://%s:%s/", u.Scheme, m.IP, out.Port)
	}
	if c, err := url.Parse(r.Caddy); err == nil && r.Caddy != "" {
		out.Caddy = c.Port()
	}
	return out
}

func main() {
	addr := flag.String("addr", "127.0.0.1:5399", "listen address")
	routes := flag.String("routes", "", "path to routes.yml (default: ../routes.yml next to the binary's directory)")
	flag.Parse()
	if *routes == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		*routes = filepath.Join(filepath.Dir(exe), "..", "routes.yml")
	}

	password := os.Getenv("DIRECTORY_PASSWORD")
	if password == "" {
		log.Fatal("DIRECTORY_PASSWORD is not set (see ./serve)")
	}
	want := token(password)

	http.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		got := token(r.PostFormValue("password"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			time.Sleep(time.Second) // slow down guessing
			showLogin(w, http.StatusUnauthorized, true)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    want,
			Path:     "/",
			MaxAge:   365 * 24 * 60 * 60,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	// Reloading after a wrong password lands here: back to the form.
	http.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	http.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, want) {
			showLogin(w, http.StatusUnauthorized, false)
			return
		}
		groups, err := load(*routes)
		if err != nil {
			log.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		total := 0
		for _, g := range groups {
			total += len(g.Rows)
		}
		err = page.Execute(&buf, map[string]any{
			"Groups":  groups,
			"Total":   total,
			"Updated": modTime(*routes),
		})
		if err != nil {
			log.Print(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(buf.Bytes())
	})

	log.Printf("directory: serving %s on %s", *routes, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func modTime(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().Format(time.DateTime)
}

var _ = strings.TrimSpace
