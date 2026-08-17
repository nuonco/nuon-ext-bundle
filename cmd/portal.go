package cmd

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2"
)

//go:embed static/index.html
var portalAssets embed.FS

type portalServer struct {
	store       day2State
	csrf        string
	requestedBy string
	hosts       map[string]bool
}

func portalCmd() *cobra.Command {
	var state, profile, region, requestedBy string
	var port int
	c := &cobra.Command{Use: "portal", Short: "serve a local day-2 operations portal", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := resolveStateFlags(&state, &profile, &region); err != nil {
			return err
		}
		store, err := makeDay2State(cmd.Context(), state, profile, region)
		if err != nil {
			return err
		}
		if requestedBy == "" {
			requestedBy = currentUsername()
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return err
		}
		defer listener.Close()
		actualPort := listener.Addr().(*net.TCPAddr).Port
		token, err := randomToken()
		if err != nil {
			return err
		}
		server := &portalServer{store: store, csrf: token, requestedBy: requestedBy, hosts: map[string]bool{fmt.Sprintf("localhost:%d", actualPort): true, fmt.Sprintf("127.0.0.1:%d", actualPort): true}}
		url := fmt.Sprintf("http://127.0.0.1:%d", actualPort)
		fmt.Fprintln(cmd.OutOrStdout(), url)
		httpServer := &http.Server{Handler: server.handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { <-cmd.Context().Done(); _ = httpServer.Close() }()
		err = httpServer.Serve(listener)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}}
	stateFlags(c, &state, &profile, &region)
	c.Flags().IntVar(&port, "port", 0, "localhost port (0 selects a random port)")
	c.Flags().StringVar(&requestedBy, "requested-by", "", "display name for portal dispatches (defaults to the OS username)")
	return c
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (p *portalServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", p.index)
	mux.HandleFunc("GET /api/catalog", p.catalog)
	mux.HandleFunc("GET /api/status", p.object("status.json"))
	mux.HandleFunc("GET /api/health", p.health)
	mux.HandleFunc("GET /api/runs", p.runs)
	mux.HandleFunc("GET /api/runs/{id}", p.run)
	mux.HandleFunc("POST /api/dispatch", p.dispatch)
	mux.HandleFunc("GET /api/dispatches", p.dispatches)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.hosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRF-Token") != p.csrf {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (p *portalServer) index(w http.ResponseWriter, _ *http.Request) {
	raw, err := portalAssets.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	raw = []byte(strings.Replace(string(raw), "{{CSRF_TOKEN}}", p.csrf, 1))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(raw)
}

func (p *portalServer) object(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok, err := p.store.Get(r.Context(), key)
		if err != nil {
			writeAPIError(w, err, 500)
			return
		}
		if !ok {
			writeAPIError(w, fmt.Errorf("not found"), 404)
			return
		}
		writeRawJSON(w, raw)
	}
}
func (p *portalServer) catalog(w http.ResponseWriter, r *http.Request) {
	p.object(day2.CatalogKey)(w, r)
}
func (p *portalServer) health(w http.ResponseWriter, r *http.Request) {
	latest, _, err := p.store.Get(r.Context(), "health/latest.json")
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	transitions, ok, err := p.store.Get(r.Context(), "health/transitions.json")
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	if !ok {
		transitions = []byte("[]")
	}
	if len(latest) == 0 {
		latest = []byte("null")
	}
	writeRawJSON(w, []byte(fmt.Sprintf(`{"latest":%s,"transitions":%s}`, latest, transitions)))
}
func (p *portalServer) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := listRuns(r.Context(), p.store)
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	writeJSON(w, runs)
}
func (p *portalServer) run(w http.ResponseWriter, r *http.Request) {
	run, err := readRun(r.Context(), p.store, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	if run == nil {
		writeAPIError(w, fmt.Errorf("run not found"), 404)
		return
	}
	writeJSON(w, run)
}
func (p *portalServer) dispatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RefID string `json:"ref_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeAPIError(w, err, 400)
		return
	}
	catalog, _, err := readCatalog(r.Context(), p.store)
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	if !catalogHasRef(catalog, input.RefID) {
		writeAPIError(w, fmt.Errorf("unknown ref %q", input.RefID), 400)
		return
	}
	id := "portal-" + strings.ToLower(ulid.Make().String())
	request := day2.Request{SchemaVersion: day2.SchemaVersion, DeploymentID: catalog.DeploymentID, BundleDigest: catalog.BundleDigest, RefID: input.RefID, DispatchID: id, Source: day2.SourcePortal, RequestedBy: p.requestedBy, CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(request)
	if err := p.store.PutIfAbsent(r.Context(), day2.RequestKey(id), raw); err != nil {
		writeAPIError(w, err, 500)
		return
	}
	writeJSON(w, map[string]string{"dispatch_id": id})
}
func (p *portalServer) dispatches(w http.ResponseWriter, r *http.Request) {
	keys, err := p.store.List(r.Context(), "dispatch/")
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	receipts := make(map[string]bool)
	for _, key := range keys {
		if strings.HasPrefix(key, day2.ReceiptsPrefix) {
			receipts[strings.TrimSuffix(strings.TrimPrefix(key, day2.ReceiptsPrefix), ".json")] = true
		}
	}
	items := make([]json.RawMessage, 0)
	for _, key := range keys {
		if !strings.HasPrefix(key, day2.RequestsPrefix) && !strings.HasPrefix(key, day2.ReceiptsPrefix) {
			continue
		}
		if strings.HasPrefix(key, day2.RequestsPrefix) && receipts[strings.TrimSuffix(strings.TrimPrefix(key, day2.RequestsPrefix), ".json")] {
			continue
		}
		raw, ok, err := p.store.Get(r.Context(), key)
		if err != nil {
			writeAPIError(w, err, 500)
			return
		}
		if ok {
			items = append(items, raw)
		}
	}
	writeJSON(w, items)
}
func writeJSON(w http.ResponseWriter, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		writeAPIError(w, err, 500)
		return
	}
	writeRawJSON(w, raw)
}
func writeRawJSON(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
func writeAPIError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
