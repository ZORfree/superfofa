package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"myproject/internal/model"
)

// 本文件实现 MCP 协议接入（第三方 FOFA 兼容平台，如 https://host/mcp）。
//
// 官方 FOFA 是 REST（GET ?email=&key=&qbase64=...），第三方平台多是 MCP：
// JSON-RPC over HTTP，POST {"method":"tools/call","params":{"name":"fofa_search",...}}，
// 鉴权走 Authorization: Bearer <token>，返回 result.content[0].text 里的 JSON 字符串。
// 两者协议不通用，所以这里单独实现一份客户端，由 api_mode 配置或 URL 形态自动判定。

const (
	apiModeAuto = "auto"
	apiModeREST = "rest"
	apiModeMCP  = "mcp"

	defaultMCPPath = "/mcp"
	mcpRPCID       = 1
)

// normalizeAPIMode 归一化配置里的协议模式；非法/空值按 auto 处理。
func normalizeAPIMode(cfg model.KeyConfig) string {
	switch strings.ToLower(strings.TrimSpace(cfg.APIMode)) {
	case apiModeREST:
		return apiModeREST
	case apiModeMCP:
		return apiModeMCP
	default:
		return apiModeAuto
	}
}

// mcpEndpoint 计算 MCP tools/call 端点。
// ok=false 表示当前配置应走官方 REST 客户端。
//
//   - api_mode=rest：永远走 REST
//   - api_mode=mcp ：强制 MCP；BaseURL 未带 /mcp 时自动补齐
//   - auto        ：BaseURL 路径以 /mcp 结尾才走 MCP（默认 https://fofa.info 保持 REST）
func mcpEndpoint(cfg model.KeyConfig) (string, bool) {
	mode := normalizeAPIMode(cfg)
	if mode == apiModeREST {
		return "", false
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = "https://fofa.info"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	path := strings.TrimRight(u.Path, "/")
	looksMCP := strings.HasSuffix(strings.ToLower(path), defaultMCPPath)
	if mode == apiModeMCP {
		if !looksMCP {
			path += defaultMCPPath
		}
		u.Path = path
		return u.String(), true
	}
	if looksMCP {
		u.Path = path
		return u.String(), true
	}
	return "", false
}

// isMCPMode 便捷判定：当前配置是否走 MCP。
func isMCPMode(cfg model.KeyConfig) bool {
	_, ok := mcpEndpoint(cfg)
	return ok
}

// mcpAPIError MCP 平台返回的业务错误（HTTP 200 + isError，或 JSON-RPC error）。
// 与网络错误区分开：业务错误不重试，直接把服务端信息透出给用户。
type mcpAPIError struct {
	Code    string
	Message string
}

func (e *mcpAPIError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "MCP 服务返回未知错误"
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		return fmt.Sprintf("%s（%s）", msg, code)
	}
	return msg
}

func isMCPAuthError(err error) bool {
	var apiErr *mcpAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToUpper(apiErr.Code)
	msg := apiErr.Error()
	return code == "AUTH_FAILED" || code == "UNAUTHORIZED" || code == "INVALID_TOKEN" ||
		strings.Contains(msg, "鉴权") || strings.Contains(msg, "Token") || strings.Contains(msg, "token")
}

// mcpClient 单个 MCP 端点 + Token 的客户端。
type mcpClient struct {
	endpoint string
	token    string
	http     *http.Client
}

func newMCPClient(cfg model.KeyConfig, endpoint, token string) *mcpClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(cfg.Proxy); p != "" {
		if pu, err := url.Parse(p); err == nil && pu.Scheme != "" && pu.Host != "" {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	return &mcpClient{
		endpoint: endpoint,
		token:    strings.TrimSpace(token),
		http:     &http.Client{Timeout: 90 * time.Second, Transport: transport},
	}
}

type mcpRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpCallContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content           []mcpCallContent `json:"content"`
	IsError           bool             `json:"isError"`
	StructuredContent json.RawMessage  `json:"structuredContent"`
}

type mcpRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Result  *mcpCallResult `json:"result"`
	Error   *mcpRPCError   `json:"error"`
}

// callTool 调用一个 MCP 工具，返回 content[0].text（通常是 JSON 字符串）。
func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) ([]byte, error) {
	if strings.TrimSpace(c.endpoint) == "" {
		return nil, fmt.Errorf("MCP 接口地址未配置")
	}
	if c.token == "" {
		return nil, fmt.Errorf("MCP Token 未配置")
	}
	payload, err := json.Marshal(mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      mcpRPCID,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("构造 MCP 请求失败: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		out, err := c.doOnce(ctx, payload)
		if err == nil {
			return out, nil
		}
		lastErr = err
		var apiErr *mcpAPIError
		// 业务错误/上下文取消不重试，只重试网络层失败
		if errors.As(err, &apiErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *mcpClient) doOnce(ctx context.Context, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造 MCP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "superfofa-mcp/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 MCP 服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 MCP 响应失败: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &mcpAPIError{Code: "AUTH_FAILED", Message: mcpAuthMessage(body)}
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("MCP 服务返回 %d: %s", resp.StatusCode, truncateForError(string(body)))
	}
	if resp.StatusCode >= 400 {
		return nil, &mcpAPIError{Code: strconv.Itoa(resp.StatusCode), Message: truncateForError(string(body))}
	}

	raw := extractJSONPayload(resp.Header.Get("Content-Type"), body)
	var rpc mcpRPCResponse
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, fmt.Errorf("解析 MCP 响应失败: %w（原始响应：%s）", err, truncateForError(string(raw)))
	}
	if rpc.Error != nil {
		return nil, &mcpAPIError{Code: strconv.Itoa(rpc.Error.Code), Message: rpc.Error.Message}
	}
	if rpc.Result == nil {
		return nil, fmt.Errorf("MCP 响应缺少 result 字段")
	}

	var sb strings.Builder
	for _, part := range rpc.Result.Content {
		if part.Type == "text" || part.Type == "" {
			sb.WriteString(part.Text)
		}
	}
	text := strings.TrimSpace(sb.String())

	if rpc.Result.IsError {
		return nil, toolError(text)
	}
	if text == "" {
		if len(rpc.Result.StructuredContent) > 0 {
			return rpc.Result.StructuredContent, nil
		}
		return []byte("{}"), nil
	}
	return []byte(text), nil
}

// mcpAuthMessage 从 401 响应体里抠出可读信息。
// 兼容两种形态：{"error":"..."} 与 JSON-RPC 的 {"error":{"code":..,"message":".."}}。
func mcpAuthMessage(body []byte) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err == nil {
		if msg := strings.TrimSpace(e.Message); msg != "" {
			return msg
		}
		if raw := bytes.TrimSpace(e.Error); len(raw) > 0 {
			if raw[0] == '"' {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
			var nested struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(raw, &nested); err == nil && strings.TrimSpace(nested.Message) != "" {
				return strings.TrimSpace(nested.Message)
			}
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return "MCP Token 无效或已过期"
	}
	return truncateForError(msg)
}

// toolError 把工具级错误文本（{"error":..,"code":..,"hint":..}）转成 mcpAPIError。
func toolError(text string) error {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Hint    string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(text), &e); err == nil {
		msg := firstNonEmpty(e.Error, e.Message)
		if msg == "" {
			msg = strings.TrimSpace(text)
		}
		if hint := strings.TrimSpace(e.Hint); hint != "" {
			msg = msg + "；" + hint
		}
		return &mcpAPIError{Code: e.Code, Message: msg}
	}
	return &mcpAPIError{Message: truncateForError(text)}
}

// extractJSONPayload 兼容两种响应体：JSON 与 SSE（text/event-stream 的 data: 行）。
func extractJSONPayload(contentType string, body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	lower := strings.ToLower(contentType)
	if !strings.Contains(lower, "event-stream") && bytes.HasPrefix(trimmed, []byte("{")) {
		return trimmed
	}
	if !strings.Contains(lower, "event-stream") {
		// 有时服务端标注不准，直接找第一条像 JSON 的行
		if idx := bytes.IndexByte(trimmed, '{'); idx > 0 {
			return bytes.TrimSpace(trimmed[idx:])
		}
		return trimmed
	}
	var picked []byte
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" || !strings.HasPrefix(data, "{") {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &probe); err != nil {
			continue
		}
		if _, ok := probe["result"]; ok {
			picked = []byte(data)
			continue
		}
		if _, ok := probe["error"]; ok {
			picked = []byte(data)
		}
	}
	if picked != nil {
		return picked
	}
	return trimmed
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// ===================== fofa_search =====================

type mcpSearchPayload struct {
	Total   int               `json:"total"`
	Size    int               `json:"size"`
	Page    int               `json:"page"`
	Fields  []string          `json:"fields"`
	Results []json.RawMessage `json:"results"`
	Data    *mcpSearchPayload `json:"data"`
}

type mcpSearchResult struct {
	Total  int
	Page   int
	Fields []string
	Rows   []map[string]string
}

// Search 调 fofa_search 工具，返回结构化行。
func (c *mcpClient) Search(ctx context.Context, query string, fields []string, page, size int) (mcpSearchResult, error) {
	args := map[string]any{"query": query}
	if len(fields) > 0 {
		args["fields"] = fields
	}
	if page > 0 {
		args["page"] = page
	}
	if size > 0 {
		args["size"] = size
	}
	raw, err := c.callTool(ctx, "fofa_search", args)
	if err != nil {
		return mcpSearchResult{}, err
	}
	return parseMCPSearchPayload(raw)
}

func parseMCPSearchPayload(raw []byte) (mcpSearchResult, error) {
	var p mcpSearchPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return mcpSearchResult{}, fmt.Errorf("解析 MCP 查询结果失败: %w（原始响应：%s）", err, truncateForError(string(raw)))
	}
	if p.Data != nil && p.Total == 0 && len(p.Results) == 0 {
		p = *p.Data
	}

	fieldNames := append([]string(nil), p.Fields...)
	rows := make([]map[string]string, 0, len(p.Results))
	for _, item := range p.Results {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 {
			continue
		}
		switch trimmed[0] {
		case '[': // 与官方一致：数组按 fields 顺序对齐
			var vals []any
			if err := json.Unmarshal(trimmed, &vals); err != nil {
				continue
			}
			row := make(map[string]string, len(fieldNames))
			for i, name := range fieldNames {
				if i < len(vals) {
					row[name] = jsonValueToString(vals[i])
				} else {
					row[name] = ""
				}
			}
			rows = append(rows, row)
		case '{': // 对象行：字段名直接来自 key
			var obj map[string]any
			if err := json.Unmarshal(trimmed, &obj); err != nil {
				continue
			}
			row := make(map[string]string, len(obj))
			keys := make([]string, 0, len(obj))
			for k, v := range obj {
				row[k] = jsonValueToString(v)
				keys = append(keys, k)
			}
			if len(fieldNames) == 0 {
				sort.Strings(keys)
				fieldNames = keys
			}
			rows = append(rows, row)
		}
	}
	if len(fieldNames) == 0 && len(rows) > 0 {
		for k := range rows[0] {
			fieldNames = append(fieldNames, k)
		}
		sort.Strings(fieldNames)
	}

	total := p.Total
	if total == 0 {
		total = p.Size
	}
	return mcpSearchResult{Total: total, Page: p.Page, Fields: fieldNames, Rows: rows}, nil
}

// ===================== fofa_stats =====================

type mcpStatsResult struct {
	Total int64
	Items []model.StatsAggItem
}

// Stats 调 fofa_stats 工具，返回某字段的 TopN 分布。
func (c *mcpClient) Stats(ctx context.Context, query, field string, size int) (mcpStatsResult, error) {
	args := map[string]any{"query": query, "field": field}
	if size > 0 {
		args["size"] = size
	}
	raw, err := c.callTool(ctx, "fofa_stats", args)
	if err != nil {
		return mcpStatsResult{}, err
	}
	return parseMCPStatsPayload(raw, size)
}

// parseMCPStatsPayload 容忍多种返回形态：
// 顶层/嵌套 items、aggs、results、distribution，甚至直接是对象数组。
func parseMCPStatsPayload(raw []byte, size int) (mcpStatsResult, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return mcpStatsResult{}, fmt.Errorf("解析 MCP 统计结果失败: %w（原始响应：%s）", err, truncateForError(string(raw)))
	}
	out := mcpStatsResult{Total: int64(readJSONInt(root, "total", "size", "count"))}
	for _, key := range []string{"items", "aggs", "results", "distribution", "list", "data"} {
		node, ok := root[key]
		if !ok {
			continue
		}
		if items, ok := decodeAggItems(node); ok && len(items) > 0 {
			out.Items = items
			if size > 0 && len(out.Items) > size {
				out.Items = out.Items[:size]
			}
			return out, nil
		}
		if isJSONObjectArray(node) {
			continue
		}
		// data 里可能再嵌一层
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(node, &inner); err != nil {
			continue
		}
		for _, innerKey := range []string{"items", "aggs", "results", "distribution", "list"} {
			if sub, ok := inner[innerKey]; ok {
				if items, ok := decodeAggItems(sub); ok && len(items) > 0 {
					out.Items = items
					if size > 0 && len(out.Items) > size {
						out.Items = out.Items[:size]
					}
					return out, nil
				}
			}
		}
	}
	if len(out.Items) == 0 {
		return mcpStatsResult{}, fmt.Errorf("MCP 统计结果格式无法识别（原始响应：%s）", truncateForError(string(raw)))
	}
	return out, nil
}

// decodeAggItems 把 [{name|value|key:..., count:...}] 或 {"xxx": n} 解析成聚合条目。
func decodeAggItems(node json.RawMessage) ([]model.StatsAggItem, bool) {
	var arr []map[string]any
	if err := json.Unmarshal(node, &arr); err == nil {
		items := make([]model.StatsAggItem, 0, len(arr))
		for _, obj := range arr {
			var item model.StatsAggItem
			for k, v := range obj {
				switch strings.ToLower(k) {
				case "name", "value", "key", "field", "label", "country", "port", "protocol", "org", "os", "server", "domain", "region", "city", "asn":
					if item.Name == "" {
						item.Name = jsonValueToString(v)
					}
				case "count", "num", "total", "size", "doc_count":
					item.Count = int64(jsonIntValue(v))
				}
			}
			if item.Name == "" && len(obj) == 1 {
				for k, v := range obj {
					item.Name = k
					item.Count = int64(jsonIntValue(v))
				}
			}
			if item.Name == "" {
				continue
			}
			items = append(items, item)
		}
		if len(items) > 0 {
			return items, true
		}
		return nil, false
	}
	var kv map[string]any
	if err := json.Unmarshal(node, &kv); err == nil {
		items := make([]model.StatsAggItem, 0, len(kv))
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, model.StatsAggItem{Name: k, Count: int64(jsonIntValue(kv[k]))})
		}
		if len(items) > 0 {
			return items, true
		}
	}
	return nil, false
}

func isJSONObjectArray(node json.RawMessage) bool {
	var arr []json.RawMessage
	if err := json.Unmarshal(node, &arr); err != nil || len(arr) == 0 {
		return false
	}
	trimmed := bytes.TrimSpace(arr[0])
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func readJSONInt(m map[string]json.RawMessage, keys ...string) int {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var f float64
			if err := json.Unmarshal(raw, &f); err == nil {
				return int(f)
			}
		}
	}
	return 0
}

func jsonIntValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

func jsonValueToString(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprint(val)
		}
		return string(b)
	}
}

// ===================== fofa_quota =====================

type mcpQuota struct {
	Balance      int  `json:"balance"`
	TodayUsed    int  `json:"todayUsed"`
	DailyLimit   int  `json:"dailyLimit"`
	ContactBound bool `json:"contactBound"`
}

// Quota 调 fofa_quota 工具（不消耗额度），用于账号检测。
func (c *mcpClient) Quota(ctx context.Context) (mcpQuota, error) {
	raw, err := c.callTool(ctx, "fofa_quota", map[string]any{})
	if err != nil {
		return mcpQuota{}, err
	}
	var q mcpQuota
	if err := json.Unmarshal(raw, &q); err != nil {
		return mcpQuota{}, fmt.Errorf("解析 MCP 额度结果失败: %w（原始响应：%s）", err, truncateForError(string(raw)))
	}
	return q, nil
}
