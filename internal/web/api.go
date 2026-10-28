package web

// The read-only status API (docs/api.md): JSON for scripts and AI agents
// ("is my blog up?"). It only reads; every change still goes through the
// dashboard. One bearer token, created and rotated on the Account page.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/im-anuvaad/phpboat/internal/auth"
	"github.com/im-anuvaad/phpboat/internal/backup"
	"github.com/im-anuvaad/phpboat/internal/edge"
	"github.com/im-anuvaad/phpboat/internal/hostinfo"
	"github.com/im-anuvaad/phpboat/internal/metrics"
	"github.com/im-anuvaad/phpboat/internal/supervisor"
)

// APIVersion is the path prefix version of the status API.
const APIVersion = "v1"

func (s *Server) apiRoutes(mux *http.ServeMux) {
	h := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, s.requireAPIToken(f)) }
	h("GET /api/v1/status", s.apiStatus)
	h("GET /api/v1/apps", s.apiApps)
	h("GET /api/v1/apps/{app}", s.apiApp)
	// Anything else under /api/ is a JSON 404 rather than the HTML login
	// redirect, so clients get an answer they can parse.
	mux.Handle("/api/", s.requireAPIToken(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			apiError(w, http.StatusMethodNotAllowed, "the API is read-only: use the dashboard to change anything")
			return
		}
		apiError(w, http.StatusNotFound, "no such endpoint; see docs/api.md")
	}))
}

// requireAPIToken authenticates with "Authorization: Bearer <token>" only.
// Session cookies are ignored, so the API never acts on a browser's
// behalf. It is not served on the setup address: only through Cloudflare
// on the dashboard domain, with the same IP filter and origin pulls as the
// dashboard.
func (s *Server) requireAPIToken(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if edge.IsDirect(r) {
			apiError(w, http.StatusForbidden, "the API is only available on the dashboard domain")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="phpboat"`)
			apiError(w, http.StatusUnauthorized, "send the API token as: Authorization: Bearer <token> (create one on the Account page)")
			return
		}
		switch err := s.Auth.CheckAPIToken(r.Context(), strings.TrimSpace(tok), edge.ClientIP(r)); {
		case errors.Is(err, auth.ErrRateLimited):
			w.Header().Set("Retry-After", "900")
			apiError(w, http.StatusTooManyRequests, err.Error())
			return
		case errors.Is(err, auth.ErrBadAPIToken):
			w.Header().Set("WWW-Authenticate", `Bearer realm="phpboat", error="invalid_token"`)
			apiError(w, http.StatusUnauthorized, err.Error())
			return
		case err != nil:
			s.Log.Error("api token check", "err", err)
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next(w, r)
	})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeJSON writes indented JSON. HTML escaping is off (it would turn "<"
// into \u003c in messages); the response is never HTML: it is served as
// application/json with nosniff.
func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		code = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString("{\"error\": \"internal error\"}\n")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------- types

// apiTime is a UTC RFC 3339 time, or null when unknown.
type apiTime time.Time

func (t apiTime) MarshalJSON() ([]byte, error) {
	if time.Time(t).IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(time.Time(t).UTC().Format(time.RFC3339))
}

// apiFloat is a number rounded to 2 decimals, or null when not a number
// (no samples yet).
type apiFloat float64

func (f apiFloat) MarshalJSON() ([]byte, error) {
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return []byte("null"), nil
	}
	return json.Marshal(math.Round(float64(f)*100) / 100)
}

type apiHost struct {
	Hostname      string   `json:"hostname"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	CPUs          int      `json:"cpus"`
	Load1         apiFloat `json:"load_1m"`
	MemoryTotal   int64    `json:"memory_total_bytes"`
	MemoryUsed    int64    `json:"memory_used_bytes"`
	MemoryUsedPct apiFloat `json:"memory_used_percent"`
	DiskTotal     int64    `json:"disk_total_bytes"`
	DiskFree      int64    `json:"disk_free_bytes"`
	DiskUsedPct   apiFloat `json:"disk_used_percent"`
}

type apiAppSummary struct {
	Name         string           `json:"name"`
	State        supervisor.State `json:"state"`
	Healthy      bool             `json:"healthy"`
	Domain       string           `json:"domain"`
	URL          string           `json:"url"`
	Build        string           `json:"build"`
	Commit       string           `json:"commit"`
	Deploying    bool             `json:"deploying"`
	NeedsRestart bool             `json:"needs_restart"`
	MemoryBytes  int64            `json:"memory_bytes"`
	MemoryLimit  int64            `json:"memory_limit_bytes"`
	Requests     int64            `json:"requests_since_start"`
	Requests5xx  int64            `json:"requests_5xx_since_start"`
	LastBackup   apiTime          `json:"last_backup_at"`
	BackupIssue  string           `json:"backup_problem"`
}

// apiPHP is the host's PHP: the version the apps run and the installed
// php-fpm package that apt keeps up to date.
type apiPHP struct {
	Version string `json:"version"`
	Package string `json:"package"`
}

type apiStatusResponse struct {
	OK       bool            `json:"ok"`
	Version  string          `json:"phpboat_version"`
	Time     apiTime         `json:"time"`
	Host     apiHost         `json:"host"`
	PHP      *apiPHP         `json:"php"`
	Summary  apiSummary      `json:"summary"`
	Warnings []string        `json:"warnings"`
	Apps     []apiAppSummary `json:"apps"`
}

type apiSummary struct {
	Apps      int `json:"apps"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

type apiAppDetail struct {
	apiAppSummary
	Repo       string         `json:"repo"`
	Process    apiProcess     `json:"process"`
	Limits     apiLimits      `json:"limits"`
	Now        *apiNow        `json:"now"`
	Deployment *apiDeployment `json:"last_deployment"`
	Backup     *apiBackup     `json:"last_backup"`
	Edge       *apiEdge       `json:"domain_status"`
	Warnings   []string       `json:"warnings"`
}

type apiProcess struct {
	PID        int     `json:"pid"`
	StartedAt  apiTime `json:"started_at"`
	HealthyAt  apiTime `json:"healthy_at"`
	Restarts   int     `json:"restarts"`
	OOMKills   int64   `json:"oom_kills"`
	LastExit   string  `json:"last_exit"`
	LastExitAt apiTime `json:"last_exit_at"`
	LastError  string  `json:"last_error"`
	NextStart  apiTime `json:"next_start_at"`
}

type apiLimits struct {
	MemoryBytes int64    `json:"memory_bytes"`
	CPUCores    apiFloat `json:"cpu_cores"`
	Processes   int      `json:"processes"`
}

type apiNow struct {
	At               apiTime  `json:"at"`
	CPUPercent       apiFloat `json:"cpu_percent_of_one_core"`
	MemoryBytes      int64    `json:"memory_bytes"`
	Processes        int64    `json:"processes"`
	RequestsPerMin   apiFloat `json:"requests_per_minute"`
	Errors5xxPerMin  apiFloat `json:"errors_5xx_per_minute"`
	LatencyP50Millis apiFloat `json:"latency_p50_ms"`
	LatencyP95Millis apiFloat `json:"latency_p95_ms"`
	DiskReadPerSec   apiFloat `json:"disk_read_bytes_per_second"`
	DiskWritePerSec  apiFloat `json:"disk_write_bytes_per_second"`
}

type apiDeployment struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	Build      string  `json:"build"`
	Commit     string  `json:"commit"`
	StartedAt  apiTime `json:"started_at"`
	FinishedAt apiTime `json:"finished_at"`
	Error      string  `json:"error"`
}

type apiBackup struct {
	ID        int64   `json:"id"`
	Kind      string  `json:"kind"`
	Status    string  `json:"status"`
	CreatedAt apiTime `json:"created_at"`
	SizeBytes int64   `json:"size_bytes"`
	InBucket  bool    `json:"in_bucket"`
	OnServer  bool    `json:"on_server"`
	Build     string  `json:"build"`
	Error     string  `json:"error"`
}

type apiEdge struct {
	DNS       string  `json:"dns"`
	CertUntil apiTime `json:"certificate_valid_until"`
	Error     string  `json:"error"`
}

// ---------------------------------------------------------------- handlers

// healthy: running, or deliberately stopped. Starting, backoff, crashed,
// or stopped although it should run are unhealthy; so is an app that was
// never deployed.
func (s *Server) appHealthy(r *http.Request, row AppRow) bool {
	switch row.Status.State {
	case supervisor.Running:
		return true
	case supervisor.Stopped:
		return row.Status.Build != "" && !s.Dep.DesiredRunning(r.Context(), row.Name)
	}
	return false
}

func (s *Server) apiSummaryOf(r *http.Request, row AppRow) apiAppSummary {
	state := row.Status.State
	if state == "" {
		state = "unknown" // stored but not loaded (it failed to load at start; see the log)
	}
	a := apiAppSummary{
		Name: row.Name, State: state, Healthy: s.appHealthy(r, row),
		Domain: row.Domain, Build: row.Status.Build, Commit: row.Commit, Deploying: row.Pending != 0,
		NeedsRestart: row.NeedsRestart, MemoryBytes: row.MemBytes, MemoryLimit: row.Limits.MemoryMax,
		Requests: row.Requests, Requests5xx: row.Requests5xx,
		LastBackup: apiTime(row.LastBackup), BackupIssue: row.BackupProblem,
	}
	if row.Domain != "" {
		a.URL = "https://" + row.Domain + "/"
	}
	return a
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := s.appRows(r.Context())
	if err != nil {
		s.apiInternal(w, err)
		return
	}
	h := hostinfo.Read(s.Layout.Root)
	resp := apiStatusResponse{
		Version: s.Version, Time: apiTime(time.Now()),
		Host: apiHost{
			Hostname: h.Hostname, UptimeSeconds: int64(h.Uptime.Seconds()), CPUs: h.CPUs, Load1: apiFloat(h.Load1),
			MemoryTotal: h.MemTotal, MemoryUsed: h.MemTotal - h.MemAvailable, MemoryUsedPct: apiFloat(h.MemUsedPct()),
			DiskTotal: h.DiskTotal, DiskFree: h.DiskFree, DiskUsedPct: apiFloat(h.DiskUsedPct()),
		},
		PHP:      s.phpInfo(),
		Warnings: nonNil(s.warnings(r, rows)),
		Apps:     []apiAppSummary{},
	}
	for _, row := range rows {
		a := s.apiSummaryOf(r, row)
		resp.Apps = append(resp.Apps, a)
		if a.Healthy {
			resp.Summary.Healthy++
		} else {
			resp.Summary.Unhealthy++
		}
	}
	resp.Summary.Apps = len(rows)
	resp.OK = resp.Summary.Unhealthy == 0
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) apiApps(w http.ResponseWriter, r *http.Request) {
	rows, err := s.appRows(r.Context())
	if err != nil {
		s.apiInternal(w, err)
		return
	}
	out := []apiAppSummary{}
	for _, row := range rows {
		out = append(out, s.apiSummaryOf(r, row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) apiApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("app")
	rows, err := s.appRows(ctx)
	if err != nil {
		s.apiInternal(w, err)
		return
	}
	var row *AppRow
	for i := range rows {
		if rows[i].Name == name {
			row = &rows[i]
		}
	}
	if row == nil {
		apiError(w, http.StatusNotFound, "no app named "+name)
		return
	}
	st := row.Status
	d := apiAppDetail{
		apiAppSummary: s.apiSummaryOf(r, *row),
		Repo:          row.Repo,
		Process: apiProcess{
			PID: st.PID, StartedAt: apiTime(st.StartedAt), HealthyAt: apiTime(st.HealthyAt), Restarts: st.Restarts,
			OOMKills: st.OOMKills, LastExit: st.LastExit, LastExitAt: apiTime(st.LastExitAt), LastError: st.LastError,
			NextStart: apiTime(st.NextStart),
		},
		Limits:   apiLimits{MemoryBytes: row.Limits.MemoryMax, CPUCores: apiFloat(row.Limits.CPUMax), Processes: row.Limits.PidsMax},
		Warnings: []string{},
	}
	if s.Metrics != nil {
		if p, ok := s.Metrics.Latest(name); ok {
			d.Now = nowOf(p)
		}
		if n := s.Metrics.OOMSince(name, time.Hour); n > 0 {
			d.Warnings = append(d.Warnings, "killed for running out of memory in the last hour")
		}
	}
	if deps, err := s.Dep.Deployments(ctx, name, 1); err == nil && len(deps) > 0 {
		x := deps[0]
		d.Deployment = &apiDeployment{ID: x.ID, Kind: x.Kind, Status: x.Status, Build: x.BuildID, Commit: x.CommitSHA,
			StartedAt: apiTime(x.StartedAt), FinishedAt: apiTime(x.FinishedAt), Error: x.Error}
	}
	if s.Backups != nil {
		if b, ok := s.Backups.Latest(ctx, name); ok {
			d.Backup = backupOf(b)
		}
	}
	if s.Edge != nil && row.Domain != "" {
		for _, h := range s.Edge.Status(ctx).Hosts {
			if h.Host == row.Domain {
				d.Edge = &apiEdge{DNS: h.DNS, CertUntil: apiTime(h.CertUntil), Error: h.Error}
			}
		}
	}
	switch {
	case st.State == supervisor.Crashed:
		d.Warnings = append(d.Warnings, "crashed and not being restarted: "+st.LastError)
	case !d.Healthy && st.Build == "":
		d.Warnings = append(d.Warnings, "never deployed")
	case !d.Healthy:
		d.Warnings = append(d.Warnings, "not running although it should be (state "+string(st.State)+")")
	}
	if row.NeedsRestart {
		d.Warnings = append(d.Warnings, "configuration changed; restart needed to apply it")
	}
	if row.BackupProblem != "" {
		d.Warnings = append(d.Warnings, row.BackupProblem)
	}
	writeJSON(w, http.StatusOK, d)
}

func nowOf(p metrics.Point) *apiNow {
	return &apiNow{
		At: apiTime(p.TS), CPUPercent: apiFloat(p.CPU), MemoryBytes: p.Mem, Processes: p.Pids,
		RequestsPerMin: apiFloat(p.Req), Errors5xxPerMin: apiFloat(p.Req5xx),
		LatencyP50Millis: apiFloat(p.P50), LatencyP95Millis: apiFloat(p.P95),
		DiskReadPerSec: apiFloat(p.IORead), DiskWritePerSec: apiFloat(p.IOWrite),
	}
}

func backupOf(b backup.Backup) *apiBackup {
	return &apiBackup{ID: b.ID, Kind: b.Kind, Status: b.Status, CreatedAt: apiTime(b.CreatedAt), SizeBytes: b.Size,
		InBucket: b.Uploaded(), OnServer: b.LocalPath != "", Build: b.BuildID, Error: b.Error}
}

func (s *Server) apiInternal(w http.ResponseWriter, err error) {
	s.Log.Error("api", "err", err)
	apiError(w, http.StatusInternalServerError, "internal error")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// phpInfo is the host's PHP for the dashboard and the status API. It is nil
// when no PHP is installed or the server has no PHP source.
func (s *Server) phpInfo() *apiPHP {
	if s.PHP == nil {
		return nil
	}
	rt, pkg := s.PHP()
	if rt.Version == "" {
		return nil
	}
	return &apiPHP{Version: rt.Version, Package: pkg}
}
