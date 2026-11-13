package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testAdminKey = "AdminKey0001"
	testUserKey  = "UserKey00002"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	keyTableMu.Lock()
	keyTable = map[string]*KeyEntry{
		testAdminKey: {ID: "admin", Key: testAdminKey, Role: "admin"},
		testUserKey:  {ID: "user01", Key: testUserKey, Role: "user"},
	}
	keyTableMu.Unlock()
	failsMu.Lock()
	fails = make(map[string]*failEntry)
	failsMu.Unlock()
	edgeIPCacheMu.Lock()
	edgeIPCache = "192.0.2.1" // keep get_status off the network
	edgeIPCacheMu.Unlock()

	mux := http.NewServeMux()
	registerAPIRoutes(mux)
	registerMCPRoutes(mux)
	registerUIRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func mcpPost(t *testing.T, srv *httptest.Server, key, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, raw
}

type testRPC struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Tools           []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func rpc(t *testing.T, srv *httptest.Server, key, body string) testRPC {
	t.Helper()
	status, raw := mcpPost(t, srv, key, body, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var out testRPC
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func callTool(t *testing.T, srv *httptest.Server, key, name, args string) testRPC {
	t.Helper()
	return rpc(t, srv, key, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
}

func TestMCPAuthAndTransport(t *testing.T) {
	srv := newTestServer(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	if s, _ := mcpPost(t, srv, "", ping, nil); s != http.StatusUnauthorized {
		t.Errorf("no key: got %d, want 401", s)
	}
	if s, _ := mcpPost(t, srv, "", ping, map[string]string{"Cookie": cookieName + "=" + testAdminKey}); s != http.StatusUnauthorized {
		t.Errorf("cookie only: got %d, want 401", s)
	}
	if s, _ := mcpPost(t, srv, testAdminKey, ping, map[string]string{"X-Forwarded-Proto": "http"}); s != http.StatusForbidden {
		t.Errorf("plain http via proxy: got %d, want 403", s)
	}
	if s, _ := mcpPost(t, srv, testAdminKey, ping, map[string]string{"X-Forwarded-Proto": "https"}); s != http.StatusOK {
		t.Errorf("https via proxy: got %d, want 200", s)
	}
	if s, _ := mcpPost(t, srv, testAdminKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); s != http.StatusAccepted {
		t.Errorf("notification: got %d, want 202", s)
	}
	res, err := srv.Client().Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", res.StatusCode)
	}
}

func TestMCPBadKeysGetRateLimited(t *testing.T) {
	srv := newTestServer(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for i := 0; i < failLimit; i++ {
		if s, _ := mcpPost(t, srv, "wrong-key-000", ping, nil); s != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, s)
		}
	}
	if s, _ := mcpPost(t, srv, testAdminKey, ping, nil); s != http.StatusTooManyRequests {
		t.Errorf("after %d bad keys: got %d, want 429", failLimit, s)
	}
}

func TestMCPInitialize(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, testUserKey, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if out.Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("known version not echoed: %q", out.Result.ProtocolVersion)
	}
	out = rpc(t, srv, testUserKey, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"2099-01-01"}}`)
	if out.Result.ProtocolVersion != mcpLatestProtocol {
		t.Errorf("unknown version: got %q, want %q", out.Result.ProtocolVersion, mcpLatestProtocol)
	}
	if string(out.ID) != `"a"` {
		t.Errorf("id not echoed: %s", out.ID)
	}
	if out := rpc(t, srv, testUserKey, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`); out.Error == nil || out.Error.Code != -32601 {
		t.Errorf("unknown method: %+v", out.Error)
	}
}

func TestMCPToolsListByRole(t *testing.T) {
	srv := newTestServer(t)
	names := func(key string) map[string]bool {
		out := rpc(t, srv, key, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		m := map[string]bool{}
		for _, tl := range out.Result.Tools {
			m[tl.Name] = true
			if tl.InputSchema["type"] != "object" {
				t.Errorf("%s: inputSchema.type = %v", tl.Name, tl.InputSchema["type"])
			}
		}
		return m
	}
	user, admin := names(testUserKey), names(testAdminKey)
	for _, n := range []string{"get_status", "list_sites", "create_site", "enable_site", "disable_site", "delete_site"} {
		if !user[n] {
			t.Errorf("user is missing tool %s", n)
		}
	}
	for _, n := range []string{"list_keys", "create_key", "delete_key"} {
		if user[n] {
			t.Errorf("user can see admin tool %s", n)
		}
		if !admin[n] {
			t.Errorf("admin is missing tool %s", n)
		}
	}
	if user["set_cf_token"] || admin["set_cf_token"] {
		t.Error("the Cloudflare token must not be settable over MCP")
	}
}

func TestMCPToolCalls(t *testing.T) {
	srv := newTestServer(t)

	out := callTool(t, srv, testUserKey, "get_status", `{}`)
	if out.Result.IsError || len(out.Result.Content) != 1 {
		t.Fatalf("get_status: %+v", out)
	}
	var st struct {
		Whoami struct {
			KeyID string `json:"key_id"`
			Role  string `json:"role"`
		} `json:"whoami"`
		CFToken struct {
			Configured bool `json:"configured"`
		} `json:"cf_token"`
		Panel panelStatus `json:"panel"`
	}
	if err := json.Unmarshal([]byte(out.Result.Content[0].Text), &st); err != nil {
		t.Fatalf("get_status payload %q: %v", out.Result.Content[0].Text, err)
	}
	if st.Whoami.KeyID != "user01" || st.Whoami.Role != "user" {
		t.Errorf("get_status identity: %+v", st.Whoami)
	}

	// Admin-only tool called with a user key is indistinguishable from unknown.
	if out := callTool(t, srv, testUserKey, "list_keys", `{}`); out.Error == nil || out.Error.Code != -32602 {
		t.Errorf("user calling list_keys: %+v", out)
	}
	if out := callTool(t, srv, testAdminKey, "list_keys", `{}`); out.Error != nil || out.Result.IsError {
		t.Errorf("admin calling list_keys: %+v", out)
	}

	if out := callTool(t, srv, testUserKey, "create_site", `{"domain":"a.example.com"}`); !out.Result.IsError ||
		!strings.Contains(out.Result.Content[0].Text, "upstream") {
		t.Errorf("missing required arg: %+v", out)
	}
	if out := callTool(t, srv, testUserKey, "disable_site", `{"domain":"nope.example.com"}`); !out.Result.IsError ||
		!strings.Contains(out.Result.Content[0].Text, "站点不存在") {
		t.Errorf("disable of unknown site: %+v", out)
	}
	traversal := `{"conf_path":"` + confDir + `/../../../../etc/vps-relay/keys.json","domain":"a.example.com","upstream":"https://o.example.com"}`
	if out := callTool(t, srv, testUserKey, "adopt_site", traversal); !out.Result.IsError ||
		!strings.Contains(out.Result.Content[0].Text, "conf_path must be") {
		t.Errorf("adopt path traversal not rejected: %+v", out)
	}
}

func TestMCPBatch(t *testing.T) {
	srv := newTestServer(t)
	status, raw := mcpPost(t, srv, testUserKey,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var out []testRPC
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 2 {
		t.Fatalf("batch reply %q: %v", raw, err)
	}
}
