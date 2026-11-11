package app

import "strings"

// splitFieldList 把逗号分隔的字段串切成去空、去重的字段列表。
func splitFieldList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		field := strings.TrimSpace(part)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}
