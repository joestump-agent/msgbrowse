// REST client tests against an httptest stub of Syncthing's API: the API-key
// header on every request, typed decode of status/version/config/completion,
// the events long-poll parameters, section put/patch bodies, and the typed
// failure modes (auth sentinel, bounded APIError, redirect-as-protocol-error).
//
// Governing: ADR-0021, SPEC-0014 REQ "Error Handling Standards" ("REST
// failure is attributable and surfaced"), SPEC-0014 Security "CSRF and
// Redirect".
package syncthing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAPIKey = "test-api-key-0123456789abcdef"

// stubServer runs an httptest server that enforces the API key and dispatches
// to the given handlers by exact path.
func stubServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testAPIKey {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(strings.TrimPrefix(srv.URL, "http://"), testAPIKey)
	return srv, client
}

func TestClientSystemStatusAndVersion(t *testing.T) {
	_, client := stubServer(t, map[string]http.HandlerFunc{
		"/rest/system/status": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("status method = %s, want GET", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"myID": "AAAA-BBBB", "uptime": 42})
		},
		"/rest/system/version": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"version": "v2.1.1", "longVersion": "syncthing v2.1.1", "os": "darwin", "arch": "universal",
			})
		},
	})
	ctx := context.Background()
	st, err := client.SystemStatus(ctx)
	if err != nil {
		t.Fatalf("SystemStatus: %v", err)
	}
	if st.MyID != "AAAA-BBBB" || st.Uptime != 42 {
		t.Errorf("SystemStatus = %+v", st)
	}
	v, err := client.SystemVersion(ctx)
	if err != nil {
		t.Fatalf("SystemVersion: %v", err)
	}
	if v.Version != "v2.1.1" || v.OS != "darwin" {
		t.Errorf("SystemVersion = %+v", v)
	}
}

func TestClientConfigRoundTrip(t *testing.T) {
	var gotFolders []FolderConfig
	var gotDevices []DeviceConfig
	var gotPatch map[string]any
	_, client := stubServer(t, map[string]http.HandlerFunc{
		"/rest/config": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(Config{
				Version: 37,
				Folders: []FolderConfig{{ID: "msgbrowse-signal", Path: "/data/archives/signal", Type: "sendreceive"}},
				Devices: []DeviceConfig{{DeviceID: "SELF", Name: "laptop"}},
			})
		},
		"/rest/config/folders": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]FolderConfig{{ID: "msgbrowse-signal"}})
			case http.MethodPut:
				if err := json.NewDecoder(r.Body).Decode(&gotFolders); err != nil {
					t.Errorf("decode put folders: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			default:
				t.Errorf("folders method = %s", r.Method)
			}
		},
		"/rest/config/devices": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]DeviceConfig{{DeviceID: "SELF"}})
			case http.MethodPut:
				if err := json.NewDecoder(r.Body).Decode(&gotDevices); err != nil {
					t.Errorf("decode put devices: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			default:
				t.Errorf("devices method = %s", r.Method)
			}
		},
		"/rest/config/devices/PEER-ID": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("patch device method = %s, want PATCH", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&gotPatch); err != nil {
				t.Errorf("decode patch: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		},
	})
	ctx := context.Background()

	cfg, err := client.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.Version != 37 || len(cfg.Folders) != 1 || cfg.Folders[0].ID != "msgbrowse-signal" {
		t.Errorf("GetConfig = %+v", cfg)
	}

	if err := client.PutFolders(ctx, []FolderConfig{{
		ID: "msgbrowse-imessage", Path: "/data/archives/imessage", Type: "sendreceive",
		Devices: []FolderDeviceRef{{DeviceID: "PEER-ID"}},
	}}); err != nil {
		t.Fatalf("PutFolders: %v", err)
	}
	if len(gotFolders) != 1 || gotFolders[0].Devices[0].DeviceID != "PEER-ID" {
		t.Errorf("put folders body = %+v", gotFolders)
	}

	if err := client.PutDevices(ctx, []DeviceConfig{{DeviceID: "PEER-ID", Name: "kitchen"}}); err != nil {
		t.Fatalf("PutDevices: %v", err)
	}
	if len(gotDevices) != 1 || gotDevices[0].Name != "kitchen" {
		t.Errorf("put devices body = %+v", gotDevices)
	}

	if err := client.PatchDevice(ctx, "PEER-ID", map[string]any{"name": "renamed"}); err != nil {
		t.Fatalf("PatchDevice: %v", err)
	}
	if gotPatch["name"] != "renamed" {
		t.Errorf("patch body = %+v", gotPatch)
	}

	folders, err := client.GetFolders(ctx)
	if err != nil || len(folders) != 1 {
		t.Errorf("GetFolders = %+v, %v", folders, err)
	}
	devices, err := client.GetDevices(ctx)
	if err != nil || len(devices) != 1 {
		t.Errorf("GetDevices = %+v, %v", devices, err)
	}
}

func TestClientEventsLongPoll(t *testing.T) {
	_, client := stubServer(t, map[string]http.HandlerFunc{
		"/rest/events": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("since") != "7" {
				t.Errorf("since = %q, want 7", q.Get("since"))
			}
			if q.Get("timeout") != "30" {
				t.Errorf("timeout = %q, want 30", q.Get("timeout"))
			}
			if q.Get("events") != "FolderCompletion,FolderSummary" {
				t.Errorf("events = %q", q.Get("events"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 8, "globalID": 8, "time": "2026-07-04T12:00:00Z",
				"type": "FolderCompletion",
				"data": map[string]any{"folder": "msgbrowse-signal", "completion": 100},
			}})
		},
	})
	events, err := client.Events(context.Background(), 7, []string{"FolderCompletion", "FolderSummary"}, 30*time.Second)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "FolderCompletion" || events[0].ID != 8 {
		t.Fatalf("events = %+v", events)
	}
	var data struct {
		Folder     string  `json:"folder"`
		Completion float64 `json:"completion"`
	}
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatalf("decode event data: %v", err)
	}
	if data.Folder != "msgbrowse-signal" || data.Completion != 100 {
		t.Errorf("event data = %+v", data)
	}
}

func TestClientFolderCompletion(t *testing.T) {
	_, client := stubServer(t, map[string]http.HandlerFunc{
		"/rest/db/completion": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("folder") != "msgbrowse-signal" || q.Get("device") != "PEER-ID" {
				t.Errorf("completion query = %v", q)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"completion": 87.5, "needBytes": 1024, "needItems": 3, "globalBytes": 8192,
			})
		},
	})
	c, err := client.FolderCompletion(context.Background(), "msgbrowse-signal", "PEER-ID")
	if err != nil {
		t.Fatalf("FolderCompletion: %v", err)
	}
	if c.CompletionPct != 87.5 || c.NeedBytes != 1024 || c.NeedItems != 3 {
		t.Errorf("completion = %+v", c)
	}
}

// TestClientTypedErrors: 401/403 match ErrAPIAuth; other non-2xx yield a
// bounded *APIError carrying operation + status + body; a redirect is a
// protocol error matching ErrUnexpectedRedirect. Nothing is swallowed.
func TestClientTypedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/system/ping":
			http.Error(w, "Forbidden", http.StatusForbidden)
		case "/rest/config":
			http.Error(w, "internal explosion", http.StatusInternalServerError)
		case "/rest/system/status":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	client := NewClient(strings.TrimPrefix(srv.URL, "http://"), "wrong-key")
	ctx := context.Background()

	err := client.Ping(ctx)
	if !errors.Is(err, ErrAPIAuth) {
		t.Errorf("Ping err = %v, want ErrAPIAuth", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("Ping err = %v, want *APIError with 403", err)
	}

	_, err = client.GetConfig(ctx)
	if !errors.As(err, &apiErr) {
		t.Fatalf("GetConfig err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError || !strings.Contains(apiErr.Body, "internal explosion") {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "get config") {
		t.Errorf("APIError.Error() = %q, want the operation name", apiErr.Error())
	}

	_, err = client.SystemStatus(ctx)
	if !errors.Is(err, ErrUnexpectedRedirect) {
		t.Errorf("SystemStatus err = %v, want ErrUnexpectedRedirect", err)
	}
}

// TestClientContextCancellation: a cancelled context aborts an in-flight
// long-poll promptly (the events worker's shutdown path in #157).
func TestClientContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the long-poll open
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	client := NewClient(strings.TrimPrefix(srv.URL, "http://"), testAPIKey)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := client.Events(ctx, 0, nil, 60*time.Second)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Events err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancellation did not abort the long-poll promptly")
	}
}
