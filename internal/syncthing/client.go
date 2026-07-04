// Loopback REST client for the supervised Syncthing daemon. Every request
// carries the msgbrowse-generated API key (X-API-Key); responses are decoded
// into typed structs; failures are typed (*APIError, sentinel-matchable) and
// never swallowed. Redirects are protocol errors (SPEC-0014 "CSRF and
// Redirect"). Request bodies are small JSON control payloads; response reads
// are bounded (SPEC-0014 "Transfer Bounds" — bulk data moves over Syncthing's
// own sync protocol, never through this client).
//
// Coverage: system status/version and ping, config get plus devices/folders
// get/put/patch (the pairing story's surface), the events long-poll (the
// folder-watch → re-ingest trigger, issue #157), and folder completion.
//
// Governing: ADR-0021 (drive Syncthing via its loopback REST API), SPEC-0014
// REQ "Error Handling Standards", SPEC-0014 Authentication (loopback +
// API key).
package syncthing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxErrorBody bounds how much of an error response body is captured into an
// *APIError (Syncthing's errors are short plain-text lines).
const maxErrorBody = 4 << 10 // 4 KiB

// maxResponseBody bounds decoded JSON responses. Config and event payloads
// are small; this guards against a protocol confusion flooding memory.
const maxResponseBody = 8 << 20 // 8 MiB

// Client is a typed client for the daemon's loopback REST API. It is safe
// for concurrent use.
type Client struct {
	base   string
	apiKey string
	hc     *http.Client
}

// NewClient builds a client for the REST API at addr (loopback host:port)
// authenticating with apiKey. The underlying http.Client carries no global
// timeout — the events long-poll must be allowed to hold a request open —
// so callers bound every call with a context.
func NewClient(addr, apiKey string) *Client {
	return &Client{
		base:   "http://" + addr,
		apiKey: apiKey,
		hc: &http.Client{
			// A redirect from the loopback daemon is a protocol error, never
			// followed (SPEC-0014 "CSRF and Redirect").
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrUnexpectedRedirect
			},
		},
	}
}

// do performs one REST call: marshal the optional request body, set the API
// key header, classify the response, and decode into out (when non-nil). All
// error paths return a wrapped, attributable error.
func (c *Client) do(ctx context.Context, op, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("syncthing REST %s: encode request: %w", op, err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("syncthing REST %s: build request: %w", op, err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// A CheckRedirect refusal surfaces here wrapped in *url.Error; keep
		// the chain so errors.Is(err, ErrUnexpectedRedirect) matches.
		return fmt.Errorf("syncthing REST %s: %w", op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		apiErr := &APIError{
			Op:         op,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(excerpt)),
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			apiErr.Err = ErrAPIAuth
		}
		return apiErr
	}
	if out == nil {
		// Drain so the connection is reusable.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("syncthing REST %s: decode response: %w", op, err)
	}
	return nil
}

// Ping confirms the daemon is up and the API key is accepted.
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		Ping string `json:"ping"`
	}
	return c.do(ctx, "ping", http.MethodGet, "/rest/system/ping", nil, &out)
}

// SystemStatus is the subset of /rest/system/status msgbrowse consumes.
type SystemStatus struct {
	// MyID is this node's own Syncthing device ID — the public identifier
	// the pairing QR will carry (SPEC-0014 "Pairing via Device ID and QR").
	MyID string `json:"myID"`
	// Uptime is the daemon's uptime in seconds.
	Uptime int64 `json:"uptime"`
}

// SystemStatus reports the daemon's status, including its own device ID.
func (c *Client) SystemStatus(ctx context.Context) (*SystemStatus, error) {
	var out SystemStatus
	if err := c.do(ctx, "system status", http.MethodGet, "/rest/system/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SystemVersion is the subset of /rest/system/version msgbrowse consumes —
// what doctor/About surface as the running engine version.
type SystemVersion struct {
	Version     string `json:"version"`
	LongVersion string `json:"longVersion"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

// SystemVersion reports the running daemon's version.
func (c *Client) SystemVersion(ctx context.Context) (*SystemVersion, error) {
	var out SystemVersion
	if err := c.do(ctx, "system version", http.MethodGet, "/rest/system/version", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeviceConfig is a Syncthing device entry as exposed by /rest/config. The
// declared fields are the ones msgbrowse owns; on put, the daemon fills
// defaults for everything omitted (msgbrowse owns the whole config, so
// defaults are always acceptable — SPEC-0014 "msgbrowse-Owned Config
// Generation").
type DeviceConfig struct {
	DeviceID  string   `json:"deviceID"`
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Paused    bool     `json:"paused,omitempty"`
}

// FolderDeviceRef references a device a folder is shared with.
type FolderDeviceRef struct {
	DeviceID string `json:"deviceID"`
}

// FolderConfig is a Syncthing folder entry as exposed by /rest/config.
type FolderConfig struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Path    string            `json:"path"`
	Type    string            `json:"type"`
	Devices []FolderDeviceRef `json:"devices,omitempty"`
	Paused  bool              `json:"paused,omitempty"`
}

// Config is the daemon configuration subset msgbrowse reads: folders and
// devices. It is read-shaped — writing goes through the section endpoints
// (PutFolders/PutDevices/PatchDevice) so a partial struct can never clobber
// unrelated daemon options with zero values.
type Config struct {
	Version int            `json:"version"`
	Folders []FolderConfig `json:"folders"`
	Devices []DeviceConfig `json:"devices"`
}

// GetConfig fetches the daemon's current configuration (folders + devices).
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	var out Config
	if err := c.do(ctx, "get config", http.MethodGet, "/rest/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDevices fetches the configured device list.
func (c *Client) GetDevices(ctx context.Context) ([]DeviceConfig, error) {
	var out []DeviceConfig
	if err := c.do(ctx, "get devices", http.MethodGet, "/rest/config/devices", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutDevices replaces the daemon's whole device list (paired peers plus the
// daemon's own entry). The pairing story adds/removes peers through this.
func (c *Client) PutDevices(ctx context.Context, devices []DeviceConfig) error {
	return c.do(ctx, "put devices", http.MethodPut, "/rest/config/devices", devices, nil)
}

// PatchDevice patches a single device entry (e.g. renaming this node). patch
// carries only the fields to change.
func (c *Client) PatchDevice(ctx context.Context, deviceID string, patch map[string]any) error {
	return c.do(ctx, "patch device "+deviceID, http.MethodPatch,
		"/rest/config/devices/"+url.PathEscape(deviceID), patch, nil)
}

// GetFolders fetches the configured folder list.
func (c *Client) GetFolders(ctx context.Context) ([]FolderConfig, error) {
	var out []FolderConfig
	if err := c.do(ctx, "get folders", http.MethodGet, "/rest/config/folders", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutFolders replaces the daemon's whole folder list. Callers pass only
// managed archive-root folders; the supervisor's config generation is the
// authority on what belongs here.
func (c *Client) PutFolders(ctx context.Context, folders []FolderConfig) error {
	return c.do(ctx, "put folders", http.MethodPut, "/rest/config/folders", folders, nil)
}

// Completion is /rest/db/completion for one folder (optionally scoped to a
// device): the authoritative "how synced is it" signal the re-ingest trigger
// and doctor read.
type Completion struct {
	// CompletionPct is the completion percentage, 0–100.
	CompletionPct float64 `json:"completion"`
	NeedBytes     int64   `json:"needBytes"`
	NeedItems     int64   `json:"needItems"`
	GlobalBytes   int64   `json:"globalBytes"`
	NeedDeletes   int64   `json:"needDeletes"`
}

// FolderCompletion reports sync completion for a folder. deviceID may be
// empty for the aggregate across devices.
func (c *Client) FolderCompletion(ctx context.Context, folderID, deviceID string) (*Completion, error) {
	q := url.Values{"folder": {folderID}}
	if deviceID != "" {
		q.Set("device", deviceID)
	}
	var out Completion
	op := "folder completion " + folderID
	if err := c.do(ctx, op, http.MethodGet, "/rest/db/completion?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConnectionInfo is one device's live connection state from
// /rest/system/connections — the peer-health signal Settings, /status, the
// CLI, and doctor surface (SPEC-0014 REQ "Status and Doctor Surfacing":
// "each paired peer's connection state").
type ConnectionInfo struct {
	// Connected reports a live P2P connection to the device.
	Connected bool `json:"connected"`
	// Paused reports the device is configured paused (no sync attempted).
	Paused bool `json:"paused"`
	// Address is the remote address of the live connection ("" when not
	// connected).
	Address string `json:"address"`
	// ClientVersion is the peer's reported Syncthing version.
	ClientVersion string `json:"clientVersion"`
}

// Connections is the /rest/system/connections response subset msgbrowse
// consumes: per-device connection state keyed by device ID.
type Connections struct {
	Connections map[string]ConnectionInfo `json:"connections"`
}

// Connections reports the daemon's per-device connection states.
func (c *Client) Connections(ctx context.Context) (*Connections, error) {
	var out Connections
	if err := c.do(ctx, "system connections", http.MethodGet, "/rest/system/connections", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FolderStatus is the /rest/db/status subset msgbrowse consumes for one
// folder: the daemon's own state token plus its error counters — how a
// paused, out-of-sync, or permission-broken folder becomes visible in
// msgbrowse's status and doctor without the user ever opening Syncthing's
// GUI (SPEC-0014 "A paused or errored sync shows in msgbrowse's status").
type FolderStatus struct {
	// State is the daemon's folder state token: "idle", "scanning",
	// "syncing", "sync-preparing", "error", "unknown", … (an open set across
	// Syncthing versions; msgbrowse maps it defensively).
	State string `json:"state"`
	// StateChanged is when the folder entered its current state.
	StateChanged time.Time `json:"stateChanged"`
	// Errors / PullErrors count failed items (permission errors, conflicts).
	Errors     int `json:"errors"`
	PullErrors int `json:"pullErrors"`
	// NeedItems / NeedBytes are the remaining delta for this node.
	NeedItems   int64 `json:"needTotalItems"`
	NeedBytes   int64 `json:"needBytes"`
	GlobalBytes int64 `json:"globalBytes"`
}

// FolderStatus reports one folder's daemon-side status.
func (c *Client) FolderStatus(ctx context.Context, folderID string) (*FolderStatus, error) {
	q := url.Values{"folder": {folderID}}
	var out FolderStatus
	op := "folder status " + folderID
	if err := c.do(ctx, op, http.MethodGet, "/rest/db/status?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Event is one entry from the daemon's event stream (/rest/events). Data is
// kept raw: each event type has its own payload shape, and the folder-watch
// story decodes only the types it subscribes to (FolderCompletion,
// FolderSummary).
type Event struct {
	ID       int64           `json:"id"`
	GlobalID int64           `json:"globalID"`
	Time     time.Time       `json:"time"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

// Events long-polls the daemon's event stream: events with ID greater than
// since, filtered to types (nil means all), holding the request open up to
// timeout when no events are pending. A nil-error empty slice means the
// long-poll timed out with nothing new — poll again with the same since.
// This is the primary re-ingest trigger feed for the folder-watch story
// (issue #157; design.md "Folder-watch trigger: REST events with an fsnotify
// fallback").
func (c *Client) Events(ctx context.Context, since int64, types []string, timeout time.Duration) ([]Event, error) {
	q := url.Values{
		"since":   {strconv.FormatInt(since, 10)},
		"timeout": {strconv.Itoa(int(timeout / time.Second))},
	}
	if len(types) > 0 {
		q.Set("events", strings.Join(types, ","))
	}
	var out []Event
	if err := c.do(ctx, "events", http.MethodGet, "/rest/events?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
