package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/HITZY2002/codearts2api/upstream"
)

func TestPanelHTTPHarness(t *testing.T) {
	if os.Getenv("CODEARTS_PANEL_HARNESS") != "1" {
		t.Skip("opt-in local browser harness")
	}
	setupModelHost(t)
	oldStats, oldCampaigns := fetchStatistics, fetchCampaigns
	fetchStatistics = func(context.Context, *Auth) (upstream.AccountStatistics, error) {
		return upstream.AccountStatistics{Package: "Local test", Credit: &upstream.CreditBalance{Total: 100, Remain: 100}}, nil
	}
	fetchCampaigns = func(context.Context, *Auth) ([]upstream.CreditCampaign, error) { return nil, nil }
	managementSettings.Lock()
	oldKey := managementSettings.key
	managementSettings.key = "local-test-key"
	managementSettings.Unlock()
	defer func() {
		fetchStatistics, fetchCampaigns = oldStats, oldCampaigns
		managementSettings.Lock()
		managementSettings.key = oldKey
		managementSettings.Unlock()
	}()
	stopped := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><title>Local CPA mock</title><iframe title="CodeArts" src="/v0/resource/plugins/codearts/panel" style="border:0;width:100%;height:95vh"></iframe>`)
			return
		}
		if r.URL.Path == "/__stop" && r.Method == http.MethodPost && r.Header.Get("Authorization") == "Bearer local-test-key" {
			_, _ = io.WriteString(w, "stopping")
			once.Do(func() { close(stopped) })
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		request, _ := json.Marshal(managementRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Headers: r.Header, Body: body})
		raw, err := managementHandle(request)
		if err != nil {
			http.Error(w, "handler failed", 500)
			return
		}
		payload, err := hostUnwrap(raw)
		if err != nil {
			http.Error(w, "envelope failed", 500)
			return
		}
		var response managementResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			http.Error(w, "decode failed", 500)
			return
		}
		for key, values := range response.Headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(response.Body)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:18765")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	t.Log("Local mock host ready on 127.0.0.1:18765")
	select {
	case <-stopped:
	case <-time.After(15 * time.Minute):
		t.Fatal("browser harness timed out")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
