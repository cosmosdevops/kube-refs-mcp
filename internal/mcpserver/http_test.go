package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cosmosdevops/kube-refs-mcp/internal/app"
)

// keysFile writes a keys file for the named holders and returns its path
// and each holder's key.
func keysFile(t *testing.T, names ...string) (path string, keys map[string]string) {
	t.Helper()
	keys = map[string]string{}
	var lines []string
	for _, name := range names {
		key, line, err := NewKey(name)
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = key
		lines = append(lines, line)
	}
	path = filepath.Join(t.TempDir(), "keys")
	content := "# test keys\n\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, keys
}

func testApp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.New(app.Options{MaxManifestBytes: app.DefaultMaxManifestBytes, MaxResources: app.DefaultMaxResources, MaxFiles: app.DefaultMaxFiles, Timeout: app.DefaultTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// hosted starts the HTTP handler the way serve --http does.
func hosted(t *testing.T, keysPath string, perMinute float64, burst int) *httptest.Server {
	t.Helper()
	keys, err := OpenKeyStore(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	_, server := New(testApp(t))
	ts := httptest.NewServer(NewHTTPHandler(server, keys, NewLimiters(perMinute, burst), 1<<20))
	t.Cleanup(ts.Close)
	return ts
}

const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// post sends one MCP request with the given Authorization header.
func post(t *testing.T, url, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(listTools))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHTTPNeedsAKey(t *testing.T) {
	path, keys := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)

	for name, authorization := range map[string]string{
		"no header":         "",
		"unknown key":       "Bearer krm_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"not a bearer":      "Basic " + keys["alice"],
		"key without krm_":  "Bearer " + strings.TrimPrefix(keys["alice"], keyPrefix),
		"the hash, not key": "Bearer " + hashKey(keys["alice"]),
	} {
		t.Run(name, func(t *testing.T) {
			resp := post(t, ts.URL, authorization)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate header")
			}
		})
	}

	if resp := post(t, ts.URL, "Bearer "+keys["alice"]); resp.StatusCode != http.StatusOK {
		t.Errorf("with a good key: status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthzIsOpen(t *testing.T) {
	path, _ := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// Each key has its own budget: one holder running out does not slow
// another down.
func TestHTTPRateLimitIsPerKey(t *testing.T) {
	path, keys := keysFile(t, "alice", "bob")
	ts := hosted(t, path, 1, 3) // 3 at once, then one a minute

	for i := range 3 {
		if resp := post(t, ts.URL, "Bearer "+keys["alice"]); resp.StatusCode != http.StatusOK {
			t.Fatalf("alice's request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := post(t, ts.URL, "Bearer "+keys["alice"])
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("alice's 4th request: status = %d, want 429", resp.StatusCode)
	}
	if wait, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || wait < 1 || wait > 60 {
		t.Errorf("Retry-After = %q, want 1 to 60 seconds", resp.Header.Get("Retry-After"))
	}
	if resp := post(t, ts.URL, "Bearer "+keys["bob"]); resp.StatusCode != http.StatusOK {
		t.Errorf("bob, after alice ran out: status = %d, want 200", resp.StatusCode)
	}
	// Requests without a key don't use up anyone's budget.
	if resp := post(t, ts.URL, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
}

func TestLimiterRefills(t *testing.T) {
	l := NewLimiters(6000, 1) // one every 10ms
	if ok, _ := l.allow("alice"); !ok {
		t.Fatal("first request refused")
	}
	ok, wait := l.allow("alice")
	if ok || wait <= 0 || wait > 20*time.Millisecond {
		t.Fatalf("second request: ok = %v, wait = %v", ok, wait)
	}
	time.Sleep(wait + 5*time.Millisecond)
	if ok, _ := l.allow("alice"); !ok {
		t.Error("still refused after waiting")
	}
}

// Keys can be added and revoked by editing the file, with no restart.
func TestKeysFileIsReloaded(t *testing.T) {
	path, keys := keysFile(t, "alice")
	store, err := OpenKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	recheck := func() { // pretend the last check was a while ago
		store.mu.Lock()
		store.checked = time.Time{}
		store.mu.Unlock()
	}
	rewrite := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// Make sure the change is visible even on a coarse file clock.
		later := time.Now().Add(time.Duration(len(content)) * time.Second)
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
		recheck()
	}

	if name, ok := store.holder(keys["alice"]); !ok || name != "alice" {
		t.Fatalf("alice's key: %q, %v", name, ok)
	}

	bobKey, bobLine, _ := NewKey("bob")
	rewrite(bobLine + "\n")
	if name, ok := store.holder(bobKey); !ok || name != "bob" {
		t.Errorf("bob's new key: %q, %v", name, ok)
	}
	if _, ok := store.holder(keys["alice"]); ok {
		t.Error("alice's key still works after it was removed")
	}

	// A broken file must not lock out the keys that were working.
	rewrite("this is not a keys file\n")
	if _, ok := store.holder(bobKey); !ok {
		t.Error("a broken keys file revoked bob's key")
	}
}

func TestKeysFileErrors(t *testing.T) {
	_, line, _ := NewKey("alice")
	for name, content := range map[string]string{
		"a raw key, not a hash": "alice krm_notahash\n",
		"three fields":          line + " extra\n",
		"bad name":              "bad/name " + strings.Fields(line)[1] + "\n",
		"same name twice":       line + "\n" + line + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenKeyStore(path); err == nil || !strings.Contains(err.Error(), "line") {
				t.Errorf("err = %v, want one naming the line", err)
			}
		})
	}
	if _, err := OpenKeyStore(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing keys file was accepted")
	}
}

func TestNewKey(t *testing.T) {
	key, line, err := NewKey("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, keyPrefix) || len(key) != len(keyPrefix)+43 {
		t.Errorf("key = %q, want krm_ and 43 characters", key)
	}
	if strings.Contains(line, key) {
		t.Error("the keys file line contains the key itself")
	}
	if line != "alice@example.com "+hashKey(key) {
		t.Errorf("line = %q", line)
	}
	other, _, _ := NewKey("alice@example.com")
	if other == key {
		t.Error("two keys came out the same")
	}
	for _, bad := range []string{"", "has space", "a/b", strings.Repeat("x", 65)} {
		if _, _, err := NewKey(bad); err == nil {
			t.Errorf("NewKey(%q) was accepted", bad)
		}
	}
}

// bearer adds an API key to every request, as an agent configured with an
// Authorization header does. extra adds further headers.
type bearer struct {
	key   string
	extra map[string]string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	for k, v := range b.extra {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens a real MCP client session over HTTP with the given key.
func connect(t *testing.T, ts *httptest.Server, key string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{key: key}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call runs a tool and decodes its structured result. It returns whether
// the tool reported an error.
func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any, out any) bool {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		return true
	}
	if out != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
	}
	return false
}

// A pod that uses a key its ConfigMap does not have.
const badPod = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\ndata: {a: b}\n---\napiVersion: v1\nkind: Pod\nmetadata: {name: web}\n" +
	"spec: {containers: [{name: c, image: nginx, env: [{name: A, valueFrom: {configMapKeyRef: {name: settings, key: missing}}}]}]}\n"

type report struct {
	Status   string `json:"status"`
	Findings []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"findings"`
}

// A real MCP client can connect over HTTP with a key and use the tools.
func TestHTTPEndToEnd(t *testing.T) {
	path, keys := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)
	session := connect(t, ts, keys["alice"])

	var graph struct {
		Links []struct {
			Status string `json:"status"`
		} `json:"links"`
	}
	if call(t, session, "describe_links", map[string]any{"manifests": badPod}, &graph) || len(graph.Links) != 1 {
		t.Fatalf("describe_links = %+v", graph)
	}
	// With no workspace root, the hosted server reads no files.
	if !call(t, session, "check_manifests", map[string]any{"files": []string{"/etc/hostname"}}, nil) {
		t.Error("the hosted server read a file")
	}
	var rep report
	if call(t, session, "check_manifests", map[string]any{"manifests": badPod}, &rep) || rep.Status != "fail" || len(rep.Findings) == 0 {
		t.Fatalf("report = %+v", rep)
	}
	// Each HTTP call stands alone, but a later call can still explain an
	// earlier check by the same key holder.
	var ex struct {
		Summary string `json:"summary"`
	}
	if call(t, session, "explain_finding", map[string]any{"findingId": rep.Findings[0].ID}, &ex) || ex.Summary == "" {
		t.Errorf("explanation = %+v", ex)
	}
}

// One key holder cannot read another holder's checks, which may hold
// details of their manifests.
func TestChecksArePrivateToTheKeyHolder(t *testing.T) {
	path, keys := keysFile(t, "alice", "bob")
	ts := hosted(t, path, 600, 100)
	alice, bob := connect(t, ts, keys["alice"]), connect(t, ts, keys["bob"])

	var rep report
	if call(t, alice, "check_manifests", map[string]any{"manifests": badPod}, &rep) || len(rep.Findings) == 0 {
		t.Fatalf("report = %+v", rep)
	}
	id := map[string]any{"findingId": rep.Findings[0].ID}
	if call(t, alice, "explain_finding", id, nil) {
		t.Error("alice cannot explain her own finding")
	}
	if !call(t, bob, "explain_finding", id, nil) {
		t.Error("bob can explain alice's finding")
	}

	// Naming another holder in a header does not help: the server sets it.
	spoof := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := spoof.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{key: keys["bob"], extra: map[string]string{holderHeader: "alice"}}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !call(t, session, "explain_finding", id, nil) {
		t.Error("bob can explain alice's finding by sending the holder header")
	}
}

func TestCheckHealth(t *testing.T) {
	path, _ := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)
	if err := CheckHealth(ts.URL + "/healthz"); err != nil {
		t.Errorf("a running server: %v", err)
	}
	if err := CheckHealth(ts.URL + "/mcp"); err == nil {
		t.Error("a 401 counted as healthy")
	}
	ts.Close()
	if err := CheckHealth(ts.URL + "/healthz"); err == nil {
		t.Error("a stopped server counted as healthy")
	}
}

// RunHTTP refuses to start without keys or with a senseless rate.
func TestRunHTTPNeedsKeys(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	if err := RunHTTP(ctx, a, HTTPOptions{Addr: "127.0.0.1:0", PerMinute: 60, Burst: 30}); err == nil || !strings.Contains(err.Error(), "--keys") {
		t.Errorf("no keys file: %v", err)
	}
	path, _ := keysFile(t, "alice")
	if err := RunHTTP(ctx, a, HTTPOptions{Addr: "127.0.0.1:0", KeysFile: path, PerMinute: 0, Burst: 30}); err == nil || !strings.Contains(err.Error(), "--rate") {
		t.Errorf("zero rate: %v", err)
	}
	if err := RunHTTP(ctx, a, HTTPOptions{Addr: "127.0.0.1:0", KeysFile: filepath.Join(t.TempDir(), "missing"), PerMinute: 60, Burst: 30}); err == nil {
		t.Error("a missing keys file was accepted")
	}
}
