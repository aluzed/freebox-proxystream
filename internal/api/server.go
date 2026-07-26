package api

import (
	"log/slog"
	"net/http"

	"github.com/aluzed/freebox-proxystream/internal/cache"
	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/scanner"
	"github.com/aluzed/freebox-proxystream/internal/subs"
	"github.com/aluzed/freebox-proxystream/internal/thumbnail"
	"github.com/aluzed/freebox-proxystream/internal/transcode"
	"github.com/aluzed/freebox-proxystream/internal/web"
)

type Server struct {
	cfg     *config.Config
	store   *db.Store
	ftp     *ftpclient.Client
	mgr     *transcode.Manager
	scanner *scanner.Scanner
	janitor *cache.Janitor
	subs    *subs.Extractor
	thumbs  *thumbnail.Generator
	tpl     *web.Templates
	log     *slog.Logger
}

type Deps struct {
	Config  *config.Config
	Store   *db.Store
	FTP     *ftpclient.Client
	Manager *transcode.Manager
	Scanner *scanner.Scanner
	Janitor *cache.Janitor
	Subs    *subs.Extractor
	Thumbs  *thumbnail.Generator
	Log     *slog.Logger
}

func New(d Deps) (*Server, error) {
	tpl, err := web.LoadTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:     d.Config,
		store:   d.Store,
		ftp:     d.FTP,
		mgr:     d.Manager,
		scanner: d.Scanner,
		janitor: d.Janitor,
		subs:    d.Subs,
		thumbs:  d.Thumbs,
		tpl:     tpl,
		log:     d.Log,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Open endpoints.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", web.StaticHandler()))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /setup", s.handleSetup)

	// Pages.
	protected := http.NewServeMux()
	protected.HandleFunc("POST /logout", s.handleLogout)
	protected.HandleFunc("GET /{$}", s.pageLibrary)
	protected.HandleFunc("GET /media/{id}", s.pageDetail)
	protected.HandleFunc("GET /watch/{id}", s.pagePlayer)
	protected.HandleFunc("GET /settings", s.pageSettings)
	protected.HandleFunc("GET /settings/libraries", s.pageSettingsLibraries)
	protected.HandleFunc("GET /settings/status", s.pageSettingsStatus)
	protected.HandleFunc("GET /settings/account", s.pageSettingsAccount)

	// htmx fragments.
	protected.HandleFunc("GET /ui/media", s.fragMediaGrid)
	protected.HandleFunc("GET /ui/media/{id}/state", s.fragMediaState)
	protected.HandleFunc("POST /ui/media/{id}/retry", s.fragRetryProbe)
	protected.HandleFunc("POST /ui/media/{id}/details", s.fragSaveDetails)
	protected.HandleFunc("POST /ui/media/{id}/details/reset", s.fragResetDetails)
	protected.HandleFunc("GET /ui/browse", s.fragBrowse)
	protected.HandleFunc("GET /ui/libraries", s.fragLibraries)
	protected.HandleFunc("POST /ui/libraries", s.fragCreateLibrary)
	protected.HandleFunc("DELETE /ui/libraries/{id}", s.fragDeleteLibrary)
	protected.HandleFunc("POST /ui/libraries/{id}/scan", s.fragScanLibrary)
	protected.HandleFunc("POST /ui/libraries/{id}/refresh", s.fragRefreshLibrary)
	protected.HandleFunc("GET /ui/status", s.fragStatus)
	protected.HandleFunc("POST /ui/cache/clear", s.fragClearCache)
	protected.HandleFunc("POST /ui/password", s.fragChangePassword)

	// JSON API.
	protected.HandleFunc("GET /api/browse", s.apiBrowse)
	protected.HandleFunc("GET /api/libraries", s.apiListLibraries)
	protected.HandleFunc("POST /api/libraries", s.apiCreateLibrary)
	protected.HandleFunc("DELETE /api/libraries/{id}", s.apiDeleteLibrary)
	protected.HandleFunc("POST /api/libraries/{id}/scan", s.apiScanLibrary)
	protected.HandleFunc("POST /api/libraries/{id}/refresh", s.apiRefreshLibrary)
	protected.HandleFunc("GET /api/media", s.apiListMedia)
	protected.HandleFunc("GET /api/media/{id}", s.apiGetMedia)
	// ServeMux wildcards have to be whole path segments, so "master.m3u8" and
	// "seg_00042.ts" are dispatched on the filename inside one handler. The
	// URLs the player sees are unchanged.
	protected.HandleFunc("GET /api/media/{id}/{file}", s.apiMediaFile)
	protected.HandleFunc("GET /api/media/{id}/subs/{file}", s.apiSubtitles)
	protected.HandleFunc("GET /api/media/{id}/poster", s.apiPoster)
	protected.HandleFunc("PUT /api/media/{id}/progress", s.apiSaveProgress)
	protected.HandleFunc("POST /api/media/{id}/retry", s.apiRetryProbe)
	protected.HandleFunc("PUT /api/media/{id}/details", s.apiSaveDetails)
	protected.HandleFunc("GET /api/status", s.apiStatus)

	mux.Handle("/", s.requireAuth(protected))

	return s.recoverPanics(s.logRequests(mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
