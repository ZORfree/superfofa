package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"myproject/internal/model"
)

func TestMCPEndpointDetection(t *testing.T) {
	cases := []struct {
		name   string
		cfg    model.KeyConfig
		wantOK bool
		want   string
	}{
		{
			name:   "default REST base url stays REST",
			cfg:    model.KeyConfig{BaseURL: "https://fofa.info"},
			wantOK: false,
		},
		{
			name:   "auto detects /mcp suffix",
			cfg:    model.KeyConfig{BaseURL: "https://fofa.as3.cc.cd/mcp"},
			wantOK: true,
			want:   "https://fofa.as3.cc.cd/mcp",
		},
		{
			name:   "auto tolerates trailing slash",
			cfg:    model.KeyConfig{BaseURL: "https://fofa.as3.cc.cd/mcp/"},
			wantOK: true,
			want:   "https://fofa.as3.cc.cd/mcp",
		},
		{
			name:   "forced mcp appends path",
			cfg:    model.KeyConfig{BaseURL: "https://fofa.as3.cc.cd", APIMode: "mcp"},
			wantOK: true,
			want:   "https://fofa.as3.cc.cd/mcp",
		},
		{
			name:   "forced rest wins over /mcp url",
			cfg:    model.KeyConfig{BaseURL: "https://fofa.as3.cc.cd/mcp", APIMode: "rest"},
			wantOK: false,
		},
		{
			name:   "forced mcp with empty base url falls back to default host",
			cfg:    model.KeyConfig{APIMode: "mcp"},
			wantOK: true,
			want:   "https://fofa.info/mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mcpEndpoint(tc.cfg)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (endpoint %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseMCPSearchPayloadArrayRows(t *testing.T) {
	raw := []byte(`{"total":123,"page":2,"fields":["host","ip","port"],"results":[["a.com:443","1.1.1.1","443"]]}`)
	res, err := parseMCPSearchPayload(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 123 || res.Page != 2 {
		t.Fatalf("total/page = %d/%d", res.Total, res.Page)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	if res.Rows[0]["host"] != "a.com:443" || res.Rows[0]["port"] != "443" {
		t.Fatalf("row = %#v", res.Rows[0])
	}
}

func TestParseMCPSearchPayloadObjectRows(t *testing.T) {
	raw := []byte(`{"total":1,"results":[{"host":"b.com","ip":"2.2.2.2","port":8080}]}`)
	res, err := parseMCPSearchPayload(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	if res.Rows[0]["port"] != "8080" {
		t.Fatalf("port = %q", res.Rows[0]["port"])
	}
	if len(res.Fields) != 3 {
		t.Fatalf("fields = %v", res.Fields)
	}
}

func TestParseMCPSearchPayloadNestedData(t *testing.T) {
	raw := []byte(`{"data":{"total":9,"fields":["ip"],"results":[["3.3.3.3"]]}}`)
	res, err := parseMCPSearchPayload(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 9 || len(res.Rows) != 1 || res.Rows[0]["ip"] != "3.3.3.3" {
		t.Fatalf("res = %#v", res)
	}
}

func TestParseMCPStatsPayloadShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"items", `{"total":100,"items":[{"name":"CN","count":70},{"name":"US","count":30}]}`, 2},
		{"top-level array", `{"aggs":[{"country":"CN","count":5}]}`, 1},
		{"map shape", `{"distribution":{"CN":7,"US":3}}`, 2},
		{"nested data", `{"data":{"items":[{"value":"nginx","count":4}]}}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseMCPStatsPayload([]byte(tc.raw), 10)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(res.Items) != tc.want {
				t.Fatalf("items = %d, want %d (%#v)", len(res.Items), tc.want, res.Items)
			}
		})
	}
}

func TestToolErrorExtractsHint(t *testing.T) {
	err := toolError(`{"error":"额度已用尽","code":"UPSTREAM_DAILY_EXHAUSTED","hint":"请明日再试"}`)
	apiErr, ok := err.(*mcpAPIError)
	if !ok {
		t.Fatalf("type = %T", err)
	}
	if apiErr.Code != "UPSTREAM_DAILY_EXHAUSTED" {
		t.Fatalf("code = %q", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "请明日再试") {
		t.Fatalf("message = %q", apiErr.Message)
	}
}

func TestExtractJSONPayloadFromSSE(t *testing.T) {
	body := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[]}}\n\n")
	got := extractJSONPayload("text/event-stream", body)
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("not json: %v (%s)", err, got)
	}
	if _, ok := probe["result"]; !ok {
		t.Fatalf("missing result: %s", got)
	}
}

func TestMcpAuthErrorDetection(t *testing.T) {
	if !isMCPAuthError(&mcpAPIError{Code: "AUTH_FAILED", Message: "鉴权失败"}) {
		t.Fatal("expected auth error")
	}
	if isMCPAuthError(&mcpAPIError{Code: "UPSTREAM_DAILY_EXHAUSTED", Message: "额度用尽"}) {
		t.Fatal("quota error must not be treated as auth error")
	}
}

func TestMCPClientSendsBothAuthHeaders(t *testing.T) {
	var gotAuth, gotMCPKey, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMCPKey = r.Header.Get("X-MCP-Key")
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"balance\":5}"}]}}`))
	}))
	defer srv.Close()

	cfg := model.KeyConfig{BaseURL: srv.URL + "/mcp", MCPKey: "mcp_test_key"}
	endpoint, ok := mcpEndpoint(cfg)
	if !ok {
		t.Fatal("endpoint not detected")
	}
	client := newMCPClient(cfg, endpoint, "fofa_mcp_test_token")
	quota, err := client.Quota(context.Background())
	if err != nil {
		t.Fatalf("quota err = %v", err)
	}
	if quota.Balance != 5 {
		t.Fatalf("balance = %d", quota.Balance)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotAuth != "Bearer fofa_mcp_test_token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotMCPKey != "mcp_test_key" {
		t.Fatalf("x-mcp-key = %q", gotMCPKey)
	}
}

func TestMCPClientOmitsEmptyMCPKey(t *testing.T) {
	var gotMCPKey string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Mcp-Key"]
		gotMCPKey = r.Header.Get("X-MCP-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	cfg := model.KeyConfig{BaseURL: srv.URL + "/mcp"}
	endpoint, _ := mcpEndpoint(cfg)
	client := newMCPClient(cfg, endpoint, "tok")
	if _, err := client.Quota(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if present {
		t.Fatalf("X-MCP-Key must not be sent when unset (got %q)", gotMCPKey)
	}
}

func TestMCPClientDetectsCloudflareBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body></body></html>`))
	}))
	defer srv.Close()

	cfg := model.KeyConfig{BaseURL: srv.URL + "/mcp"}
	endpoint, _ := mcpEndpoint(cfg)
	client := newMCPClient(cfg, endpoint, "tok")
	_, err := client.Quota(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	blocked, ok := err.(*mcpBlockedError)
	if !ok {
		t.Fatalf("type = %T (%v)", err, err)
	}
	if blocked.HasMCPKey {
		t.Fatal("HasMCPKey should be false")
	}
	if !strings.Contains(err.Error(), "MCP Key") {
		t.Fatalf("message should hint at MCP Key: %v", err)
	}
}

// 平台真实返回形态：columns + rows（对象数组），见 fofa.as3.cc.cd
func TestParseMCPSearchPayloadPlatformShape(t *testing.T) {
	raw := []byte(`{
	  "query": "ip=\"1.1.1.1\"",
	  "total": 75300,
	  "page": 1,
	  "size": 2,
	  "columns": ["host","ip","port","title"],
	  "rows": [
	    {"host":"a.example:2082","ip":"1.1.1.1","port":"2082","title":""},
	    {"host":"b.example","ip":"1.1.1.1","port":"80"}
	  ],
	  "maxRows": 10000,
	  "truncated": true,
	  "returned": 2
	}`)
	res, err := parseMCPSearchPayload(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 75300 || res.Page != 1 {
		t.Fatalf("total/page = %d/%d", res.Total, res.Page)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	if strings.Join(res.Fields, ",") != "host,ip,port,title" {
		t.Fatalf("fields = %v", res.Fields)
	}
	if res.Rows[1]["title"] != "" {
		t.Fatalf("missing column should be empty, got %q", res.Rows[1]["title"])
	}
	if res.Rows[0]["host"] != "a.example:2082" {
		t.Fatalf("host = %q", res.Rows[0]["host"])
	}
}

// 平台真实统计形态：distinct + distribution
func TestParseMCPStatsPayloadPlatformShape(t *testing.T) {
	raw := []byte(`{
	  "query": "ip=\"1.1.1.1\"",
	  "field": "port",
	  "total": 75300,
	  "distinct": {"ip": 1, "ipc": 0},
	  "distribution": [
	    {"value":"80","name":"80","count":39841},
	    {"value":"53","name":"53","count":13509}
	  ]
	}`)
	res, err := parseMCPStatsPayload(raw, 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 75300 {
		t.Fatalf("total = %d", res.Total)
	}
	if res.Distinct != 1 {
		t.Fatalf("distinct = %d", res.Distinct)
	}
	if len(res.Items) != 2 || res.Items[0].Name != "80" || res.Items[0].Count != 39841 {
		t.Fatalf("items = %#v", res.Items)
	}
}
