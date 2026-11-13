// MCP endpoint (Streamable HTTP, stateless, JSON responses only).
//
// POST /mcp with `Authorization: Bearer <key>` speaks JSON-RPC 2.0 and exposes
// the panel's REST API as MCP tools. Each tool call is replayed in-process
// against the same mux the web UI uses, so authorisation, ownership checks and
// rollback logic live in exactly one place (api.go).
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const mcpLatestProtocol = "2025-06-18"

var mcpProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

const mcpInstructions = "vps-relay 反代加速面板。每个站点 = 一个加速域名反代到一个上游 URL；" +
	"create_site 会自动创建 Cloudflare DNS A 记录、签发证书并重载 Nginx，可能耗时 1-3 分钟。" +
	"Cloudflare Token 只能在网页面板里配置；先用 get_status 确认已配置。" +
	"普通用户只能看到和操作自己的站点，管理员可操作全部。"

var apiMux *http.ServeMux

func registerMCPRoutes(mux *http.ServeMux) {
	apiMux = mux
	mux.HandleFunc("/mcp", mcpHandler)
}

// ---------------- JSON-RPC plumbing ----------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var rpcNullID = json.RawMessage("null")

func rpcFail(id json.RawMessage, code int, msg string) *rpcResponse {
	if len(id) == 0 {
		id = rpcNullID
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func mcpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream and no sessions: GET/DELETE are not offered.
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Requests proxied by the host nginx carry X-Forwarded-Proto. Keys must not
	// travel over plain HTTP; direct loopback access (no header) stays allowed.
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" && p != "https" {
		writeErr(w, http.StatusForbidden,
			"MCP 仅允许通过 HTTPS 访问：请先由管理员在面板保存 Cloudflare Token，面板会自动切换到 HTTPS")
		return
	}

	ip := clientIP(r)
	if blocked, _ := isBlocked(ip); blocked {
		writeErr(w, http.StatusTooManyRequests, "认证失败次数过多，请稍后再试")
		return
	}
	// Header only — never the browser cookie — so a web page cannot drive /mcp.
	key := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	entry := lookupKey(key)
	if entry == nil || !tokenEq(key, entry.Key) {
		if key != "" {
			recordFail(ip)
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="vps-relay"`)
		writeErr(w, http.StatusUnauthorized, "未授权访问：需要 Authorization: Bearer <密钥>")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcFail(nil, -32700, "read error"))
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, rpcFail(nil, -32700, "empty body"))
		return
	}

	if body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 {
			writeJSON(w, http.StatusBadRequest, rpcFail(nil, -32700, "parse error"))
			return
		}
		var out []*rpcResponse
		for _, raw := range batch {
			if resp := mcpDispatch(entry, raw); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	resp := mcpDispatch(entry, body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpDispatch handles one JSON-RPC message. It returns nil for notifications
// and for responses sent by the client, which get no reply.
func mcpDispatch(entry *KeyEntry, raw json.RawMessage) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcFail(nil, -32700, "parse error")
	}
	if req.JSONRPC != "2.0" {
		return rpcFail(req.ID, -32600, "invalid request: jsonrpc must be \"2.0\"")
	}
	if req.Method == "" {
		return nil
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}
	ok := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := mcpLatestProtocol
		if mcpProtocols[p.ProtocolVersion] {
			ver = p.ProtocolVersion
		}
		return ok(map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vps-relay", "version": version},
			"instructions":    mcpInstructions,
		})

	case "ping":
		return ok(map[string]any{})

	case "tools/list":
		list := []map[string]any{}
		for _, t := range mcpTools {
			if t.Admin && entry.Role != "admin" {
				continue
			}
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Desc,
				"inputSchema": mcpSchema(t),
				"annotations": map[string]any{
					"readOnlyHint":    t.ReadOnly,
					"destructiveHint": t.Destructive,
					"idempotentHint":  t.Idempotent,
					"openWorldHint":   true,
				},
			})
		}
		return ok(map[string]any{"tools": list})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcFail(req.ID, -32602, "invalid params")
		}
		tool := mcpFindTool(p.Name)
		if tool == nil || (tool.Admin && entry.Role != "admin") {
			return rpcFail(req.ID, -32602, "unknown tool: "+p.Name)
		}
		args := mcpArgs{}
		for _, a := range tool.Args {
			v, _ := p.Arguments[a.Name].(string)
			v = strings.TrimSpace(v)
			if v == "" && a.Required {
				return ok(mcpText("缺少必填参数: "+a.Name, true))
			}
			args[a.Name] = v
		}
		status, out := tool.Run(entry.Key, args)
		text := strings.TrimSpace(string(out))
		if text == "" {
			text = http.StatusText(status)
		}
		return ok(mcpText(text, status >= 400))

	default:
		return rpcFail(req.ID, -32601, "method not found: "+req.Method)
	}
}

func mcpText(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// ---------------- in-process REST replay ----------------

type memResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (m *memResponse) Header() http.Header         { return m.header }
func (m *memResponse) WriteHeader(code int)        { m.status = code }
func (m *memResponse) Write(b []byte) (int, error) { return m.buf.Write(b) }

func apiCall(key, method, path string, body any) (int, []byte) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return http.StatusBadRequest, []byte(`{"error":"invalid arguments"}`)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, path, rd)
	if err != nil {
		return http.StatusBadRequest, []byte(`{"error":"invalid arguments"}`)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "127.0.0.1:0"
	rec := &memResponse{header: http.Header{}, status: http.StatusOK}
	apiMux.ServeHTTP(rec, req)
	return rec.status, rec.buf.Bytes()
}

// ---------------- tool table ----------------

type mcpArgs map[string]string

type mcpArg struct {
	Name     string
	Desc     string
	Required bool
	Enum     []string
}

type mcpTool struct {
	Name        string
	Desc        string
	Args        []mcpArg
	Admin       bool // listed and callable for admin keys only
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Run         func(key string, a mcpArgs) (int, []byte)
}

func mcpSchema(t mcpTool) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, a := range t.Args {
		p := map[string]any{"type": "string", "description": a.Desc}
		if len(a.Enum) > 0 {
			p["enum"] = a.Enum
		}
		props[a.Name] = p
		if a.Required {
			required = append(required, a.Name)
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func mcpFindTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

func sitePath(a mcpArgs, suffix string) string {
	return "/api/sites/" + url.PathEscape(strings.ToLower(a["domain"])) + suffix
}

var (
	argDomain   = mcpArg{Name: "domain", Desc: "加速域名，例如 app.example.com", Required: true}
	argUpstream = mcpArg{Name: "upstream", Desc: "上游 URL，http(s)://主机[:端口]，例如 https://origin.example.com", Required: true}
)

var mcpTools = []mcpTool{
	{
		Name:     "get_status",
		Desc:     "查看当前密钥身份(key_id/role)、Cloudflare Token 是否已配置、边缘 IPv4、面板域名与 HTTPS 状态。开始操作前先调用。",
		ReadOnly: true, Idempotent: true,
		Run: func(key string, _ mcpArgs) (int, []byte) {
			out := map[string]json.RawMessage{}
			for name, path := range map[string]string{
				"whoami":   "/api/whoami",
				"cf_token": "/api/cf-token/status",
				"edge":     "/api/edge",
				"panel":    "/api/panel",
			} {
				status, body := apiCall(key, http.MethodGet, path, nil)
				if status >= 400 {
					return status, body
				}
				out[name] = json.RawMessage(bytes.TrimSpace(body))
			}
			b, err := json.Marshal(out)
			if err != nil {
				return http.StatusInternalServerError, []byte(`{"error":"marshal status"}`)
			}
			return http.StatusOK, b
		},
	},
	{
		Name:     "list_zones",
		Desc:     "列出当前密钥的 Cloudflare Token 可管理的 Zone。加速域名必须属于其中之一。",
		ReadOnly: true, Idempotent: true,
		Run: func(key string, _ mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodGet, "/api/zones", nil)
		},
	},
	{
		Name:     "list_sites",
		Desc:     "列出站点及其启用状态、证书剩余天数、DNS 解析、上游/边缘可达性。管理员看到全部，普通用户只看到自己的。",
		ReadOnly: true, Idempotent: true,
		Run: func(key string, _ mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodGet, "/api/sites", nil)
		},
	},
	{
		Name: "create_site",
		Desc: "新建反代站点：创建 Cloudflare A 记录(指向本机边缘 IP)、签发 Let's Encrypt 证书、写入 Nginx 配置并重载。" +
			"同步执行，可能耗时 1-3 分钟；失败会自动回滚。域名已存在时返回冲突错误。",
		Args: []mcpArg{argDomain, argUpstream},
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodPost, "/api/sites", map[string]string{
				"domain": a["domain"], "upstream": a["upstream"],
			})
		},
	},
	{
		Name:       "update_site_upstream",
		Desc:       "修改已启用站点的上游 URL，校验 Nginx 配置后重载，失败自动恢复原配置。",
		Args:       []mcpArg{argDomain, argUpstream},
		Idempotent: true,
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodPut, sitePath(a, ""), map[string]string{"upstream": a["upstream"]})
		},
	},
	{
		Name:       "enable_site",
		Desc:       "启用一个已停用的站点。已启用时不做任何改动。",
		Args:       []mcpArg{argDomain},
		Idempotent: true,
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodPost, sitePath(a, "/enable"), nil)
		},
	},
	{
		Name:       "disable_site",
		Desc:       "停用站点(保留配置、DNS 和证书，可再启用)。已停用时不做任何改动。",
		Args:       []mcpArg{argDomain},
		Idempotent: true,
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodPost, sitePath(a, "/disable"), nil)
		},
	},
	{
		Name:        "delete_site",
		Desc:        "永久删除站点：移除 Nginx 配置，并清理对应的 Cloudflare DNS 记录和证书。不可恢复，执行前应得到用户明确确认。",
		Args:        []mcpArg{argDomain},
		Destructive: true,
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodDelete, sitePath(a, ""), nil)
		},
	},
	{
		Name: "adopt_site",
		Desc: "把 conf.d 目录下一个手写的 Nginx 配置接管为托管站点(加上 relay-meta 头并重命名为 relay-<domain>.conf，原文件留 .pre-adopt.bak 备份)。" +
			"不会签发证书或改动 DNS。",
		Args: []mcpArg{
			{Name: "conf_path", Desc: "现有配置的绝对路径，必须是 " + confDir + "/ 下的 .conf 文件", Required: true},
			argDomain,
			argUpstream,
			{Name: "cf_zone", Desc: "可选：该域名所在的 Cloudflare Zone 名"},
			{Name: "cf_record_id", Desc: "可选：对应 A 记录的 Cloudflare record id"},
		},
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodPost, "/api/sites/adopt", map[string]string{
				"conf_path": a["conf_path"], "domain": a["domain"], "upstream": a["upstream"],
				"cf_zone": a["cf_zone"], "cf_record_id": a["cf_record_id"],
			})
		},
	},
	{
		Name:     "list_keys",
		Desc:     "列出全部登录密钥(密钥本身脱敏)。仅管理员。",
		Admin:    true,
		ReadOnly: true, Idempotent: true,
		Run: func(key string, _ mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodGet, "/api/keys", nil)
		},
	},
	{
		Name:  "create_key",
		Desc:  "创建一个新的登录密钥并返回明文(只显示这一次)。该用户需自行登录网页面板配置 Cloudflare Token。仅管理员。",
		Admin: true,
		Args: []mcpArg{
			{Name: "id", Desc: "密钥 ID，例如 user01，不可与现有重复", Required: true},
			{Name: "role", Desc: "角色，默认 user", Enum: []string{"user", "admin"}},
			{Name: "note", Desc: "可选备注"},
		},
		Run: func(key string, a mcpArgs) (int, []byte) {
			role := a["role"]
			if role == "" {
				role = "user"
			}
			return apiCall(key, http.MethodPost, "/api/keys", map[string]string{
				"id": a["id"], "role": role, "note": a["note"],
			})
		},
	},
	{
		Name:        "delete_key",
		Desc:        "删除一个登录密钥，该密钥立即失效；其名下站点保留。不能删除最后一个管理员密钥。仅管理员。",
		Admin:       true,
		Args:        []mcpArg{{Name: "id", Desc: "要删除的密钥 ID", Required: true}},
		Destructive: true,
		Run: func(key string, a mcpArgs) (int, []byte) {
			return apiCall(key, http.MethodDelete, "/api/keys/"+url.PathEscape(a["id"]), nil)
		},
	},
}
