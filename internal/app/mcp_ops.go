package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"myproject/internal/model"
)

// 本文件把 MCP 客户端接到 App 的业务方法上：
// 搜索、统计、导出分页、账号检测（额度查询）。

// translateMCPError 统一错误文案：业务错误原样透出，网络错误加上排查提示。
func translateMCPError(err error) error {
	if err == nil {
		return nil
	}
	if apiErr, ok := err.(*mcpAPIError); ok {
		return fmt.Errorf("%s", apiErr.Error())
	}
	return fmt.Errorf("连接 MCP 平台失败，请检查 API 地址/代理后重试：%s", err.Error())
}

// searchFofaMCP 通过 MCP 平台执行一次查询。
func (a *App) searchFofaMCP(ctx context.Context, cfg model.KeyConfig, acc model.FofaAccount, query string, fields []string, page, size int) (model.SearchFofaResult, error) {
	endpoint, ok := mcpEndpoint(cfg)
	if !ok {
		return model.SearchFofaResult{}, fmt.Errorf("当前配置未启用 MCP 协议")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := newMCPClient(cfg, endpoint, acc.Key)

	start := time.Now()
	res, err := client.Search(ctx, query, fields, page, size)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return model.SearchFofaResult{}, translateMCPError(err)
	}

	return model.SearchFofaResult{
		Query:        query,
		Mode:         "mcp",
		Page:         page,
		PageSize:     size,
		Total:        res.Total,
		Returned:     len(res.Rows),
		Fields:       res.Fields,
		Results:      res.Rows,
		AccountID:    acc.ID,
		AccountEmail: acc.Email,
		ElapsedMs:    elapsed,
	}, nil
}

// exportPageMCP 导出的单页查询（MCP 实现）。
func (a *App) exportPageMCP(ctx context.Context, cfg model.KeyConfig, acc model.FofaAccount, query string, fields []string, page, size int) (exportSearchPageResult, error) {
	endpoint, ok := mcpEndpoint(cfg)
	if !ok {
		return exportSearchPageResult{}, fmt.Errorf("当前配置未启用 MCP 协议")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := newMCPClient(cfg, endpoint, acc.Key)
	res, err := client.Search(ctx, query, fields, page, size)
	if err != nil {
		return exportSearchPageResult{}, translateMCPError(err)
	}
	return exportSearchPageResult{Rows: res.Rows}, nil
}

// statsFofaMCP 通过 MCP 平台做聚合统计。
// MCP 的 fofa_stats 一次只接受一个聚合字段，这里按字段逐个调用再合并。
func (a *App) statsFofaMCP(ctx context.Context, cfg model.KeyConfig, acc model.FofaAccount, query string, fields []string, size int) (model.StatsFofaResult, error) {
	endpoint, ok := mcpEndpoint(cfg)
	if !ok {
		return model.StatsFofaResult{}, fmt.Errorf("当前配置未启用 MCP 协议")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := newMCPClient(cfg, endpoint, acc.Key)

	start := time.Now()
	aggs := make(map[string][]model.StatsAggItem, len(fields))
	returned := make(map[string]int, len(fields))
	distinct := make(map[string]int64, len(fields))
	var total int64
	for i, field := range fields {
		if i > 0 {
			// 与官方 stats 保持同样的节流节奏，避免触发上游限流
			select {
			case <-ctx.Done():
				return model.StatsFofaResult{}, ctx.Err()
			case <-time.After(statsThrottleGap):
			}
		}
		res, err := client.Stats(ctx, query, field, size)
		if err != nil {
			return model.StatsFofaResult{}, translateMCPError(err)
		}
		aggs[field] = res.Items
		returned[field] = len(res.Items)
		distinct[field] = int64(len(res.Items))
		if res.Total > total {
			total = res.Total
		}
	}

	return model.StatsFofaResult{
		Query:        query,
		Fields:       fields,
		Size:         size,
		Total:        total,
		Returned:     returned,
		Distinct:     distinct,
		Aggs:         aggs,
		AccountID:    acc.ID,
		AccountEmail: acc.Email,
		ElapsedMs:    time.Since(start).Milliseconds(),
	}, nil
}

// probeFofaMCP 用 fofa_quota 检测 MCP Token（不消耗查询额度）。
func (a *App) probeFofaMCP(cfg model.KeyConfig, key string) (status, errMsg string, snap *model.AccountSnapshot) {
	endpoint, ok := mcpEndpoint(cfg)
	if !ok {
		return model.StatusError, "当前配置未启用 MCP 协议", nil
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	client := newMCPClient(cfg, endpoint, key)
	quota, err := client.Quota(ctx)
	if err != nil {
		if isMCPAuthError(err) {
			return model.StatusInvalid, strings.TrimSpace(err.Error()), nil
		}
		return model.StatusError, err.Error(), nil
	}
	snapshot := &model.AccountSnapshot{
		Username:       "MCP 平台 Token",
		Category:       "MCP",
		RemainAPIQuery: quota.Balance,
		IsVerified:     quota.ContactBound,
	}
	return model.StatusValid, "", snapshot
}
