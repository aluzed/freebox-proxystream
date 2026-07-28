// Package web holds the embedded frontend: Go templates rendered server side
// and driven by htmx, plus the vendored hls.js and htmx bundles. No build step.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Templates struct {
	pages     map[string]*template.Template
	fragments *template.Template
}

// assetVersion is a digest of everything under static/, computed once at
// startup. It is appended to asset URLs so a new binary serves new URLs.
// Without it the year-long cache header means a stylesheet change is invisible
// until the user thinks to hard-refresh, which is not a thing to ask of them.
var assetVersion = computeAssetVersion()

func computeAssetVersion() string {
	h := sha256.New()
	entries, err := fs.Glob(staticFS, "static/*")
	if err != nil {
		return "dev"
	}
	sort.Strings(entries)
	for _, name := range entries {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write([]byte(name))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Asset returns a cache-busting URL for a file under static/.
func Asset(name string) string {
	return "/static/" + name + "?v=" + assetVersion
}

func AssetVersion() string { return assetVersion }

var funcs = template.FuncMap{
	"duration":    FormatDuration,
	"bytes":       FormatBytes,
	"timeAgo":     TimeAgo,
	"pct":         Percent,
	"pct64":       PercentInt,
	"asset":       Asset,
	"statusLabel": StatusLabel,
	"hasPrefix":   strings.HasPrefix,
	"deref":       Deref,
	"deref64":     Deref64,
	"derefStr":    DerefStr,
	"add":         func(a, b int) int { return a + b },
	"seq":         func(n int) []struct{} { return make([]struct{}, n) },
	"dict":        Dict,
	"parentDir":   func(p string) string { return path.Dir(p) },
}

func LoadTemplates() (*Templates, error) {
	fragments, err := template.New("fragments").Funcs(funcs).
		ParseFS(templateFS, "templates/fragments/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse fragments: %w", err)
	}

	entries, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}

	pages := make(map[string]*template.Template, len(entries))
	for _, entry := range entries {
		name := path.Base(entry)
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html",
			"templates/fragments/*.html",
			entry,
		)
		if err != nil {
			return nil, fmt.Errorf("parse page %s: %w", name, err)
		}
		pages[name] = t
	}

	return &Templates{pages: pages, fragments: fragments}, nil
}

func (t *Templates) Render(w io.Writer, page string, data any) error {
	tpl, ok := t.pages[page]
	if !ok {
		return fmt.Errorf("unknown page template %q", page)
	}
	return tpl.ExecuteTemplate(w, "layout.html", data)
}

// RenderFragment renders a single htmx-swappable partial.
func (t *Templates) RenderFragment(w io.Writer, name string, data any) error {
	return t.fragments.ExecuteTemplate(w, name+".html", data)
}

func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	h := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe to cache hard only because the URL carries a digest of the
		// content: a changed file is a changed URL.
		if r.URL.Query().Get("v") == assetVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func FormatDuration(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02d", h, m)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d o", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %co", float64(b)/float64(div), "kMGTP"[exp])
}

func TimeAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "jamais"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "à l'instant"
	case d < time.Hour:
		return fmt.Sprintf("il y a %d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("il y a %d h", int(d.Hours()))
	default:
		return fmt.Sprintf("il y a %d j", int(d.Hours()/24))
	}
}

func Percent(a, b int64) int {
	if b <= 0 {
		return 0
	}
	p := int(a * 100 / b)
	if p > 100 {
		return 100
	}
	return p
}

// PercentInt is the same as Percent for plain counters.
func PercentInt(a, b int) int {
	if b <= 0 {
		return 0
	}
	p := a * 100 / b
	if p > 100 {
		return 100
	}
	return p
}

// StatusLabel turns the stored status into something a person can act on.
func StatusLabel(status string) string {
	switch status {
	case "new":
		return "en file d'attente d'analyse"
	case "probing":
		return "analyse en cours"
	case "ready":
		return "prêt"
	case "error":
		return "erreur"
	}
	return status
}

func Deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func DerefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Dict builds a map inline, so a fragment can be invoked with extra context.
func Dict(pairs ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}
		m[key] = pairs[i+1]
	}
	return m
}

func Deref64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
