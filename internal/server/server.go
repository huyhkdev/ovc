// Package server holds the OVC Server HTTP API (spec §8).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"ovc/internal/catalog"
	"ovc/internal/ci"
	"ovc/internal/config"
	"ovc/internal/githost"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/store"
)

// Version is set at build time with -ldflags.
var Version = "0.1.0-dev"

// Deps are the collaborators of the API; tests replace them with fakes.
type Deps struct {
	Cfg     *config.Server
	Git     *gitops.Repo
	Store   store.Store
	Catalog catalog.Catalog
	Host    githost.Host // opens pull/merge requests; nil when the remote has no host API
	Jobs    *jobs.Manager
	Log     *slog.Logger
	// BaseCtx bounds the lifetime of background jobs (not of a request).
	BaseCtx context.Context

	// beforePush, when set (tests only), runs right before every push.
	beforePush func(branch string)
}

type Server struct {
	Deps
	ciSettings ci.Settings
	// initializing holds schemas whose init job is running, so two requests
	// for the same schema cannot race.
	initializing initSet
}

func New(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.BaseCtx == nil {
		d.BaseCtx = context.Background()
	}
	s := &Server{Deps: d, initializing: newInitSet()}
	if d.Cfg != nil {
		s.ciSettings = ci.Settings{
			GitHubTemplate: d.Cfg.CI.GitHubTemplate,
			GitLabProject:  d.Cfg.CI.GitLabProject,
			GitLabFile:     d.Cfg.CI.GitLabFile,
		}
	}
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/version", s.handleVersion)
		r.Group(func(r chi.Router) {
			r.Use(s.requireClient)
			r.Get("/schemas", s.handleListSchemas)
			r.Post("/schemas", s.handleInitSchema)
			r.Post("/schemas/{schema}/hydrate", s.handleHydrate)
			r.Get("/jobs/{id}", s.handleGetJob)
		})
	})
	return r
}

// ---- errors (spec §8: {"error":{"code","message","details"}}) -------------

type apiError struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *apiError) {
	body := map[string]any{"code": e.Code, "message": e.Message}
	if e.Details != nil {
		body["details"] = e.Details
	}
	writeJSON(w, e.Status, map[string]any{"error": body})
}

// ---- identity and client version -------------------------------------------

type identityKey struct{}

// Identity is the declared (not authenticated) author of a request (spec §3.5, §8).
type Identity struct{ Name, Email string }

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}

// requireClient enforces X-OVC-Client-Version (426 when too old) and the
// declared identity headers.
func (s *Server) requireClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("X-OVC-Client-Version")
		if v == "" {
			writeError(w, &apiError{400, "CLIENT_VERSION_REQUIRED", "header X-OVC-Client-Version is required", nil})
			return
		}
		if s.Cfg != nil && compareVersions(v, s.Cfg.CLI.MinVersion) < 0 {
			writeError(w, &apiError{http.StatusUpgradeRequired, "CLIENT_TOO_OLD",
				fmt.Sprintf("client %s is older than the minimum %s; run `ovc update`", v, s.Cfg.CLI.MinVersion),
				map[string]string{"min_version": s.Cfg.CLI.MinVersion}})
			return
		}
		name, email := strings.TrimSpace(r.Header.Get("X-OVC-User-Name")), strings.TrimSpace(r.Header.Get("X-OVC-User-Email"))
		if name == "" || email == "" || strings.ContainsAny(name+email, "<>\r\n") || !strings.Contains(email, "@") {
			writeError(w, &apiError{400, "IDENTITY_REQUIRED",
				"set your identity first: ovc config user.name \"...\" && ovc config user.email \"...\"", nil})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, Identity{name, email})))
	})
}

// compareVersions compares dotted numbers, ignoring any "-suffix": 0.1.0-dev == 0.1.0.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// ---- simple handlers --------------------------------------------------------

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	minV := "0.0.0"
	if s.Cfg != nil {
		minV = s.Cfg.CLI.MinVersion
	}
	writeJSON(w, 200, map[string]string{"server_version": Version, "min_cli_version": minV})
}

func (s *Server) handleListSchemas(w http.ResponseWriter, _ *http.Request) {
	list := s.Store.List()
	if list == nil {
		list = []store.Schema{}
	}
	writeJSON(w, 200, map[string]any{"schemas": list})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Jobs.Get(chi.URLParam(r, "id"))
	if !ok {
		writeError(w, &apiError{404, "JOB_NOT_FOUND", "no such job", nil})
		return
	}
	writeJSON(w, 200, j)
}
