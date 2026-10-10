package pool

import "strings"

// 格式校验不赋予信任；调用方仍须只传宿主提供的 TraceID。
func NormalizeTraceID(id string) string {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return ""
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", ch) {
			return ""
		}
	}
	if !strings.ContainsRune("12345678", rune(id[14])) || !strings.ContainsRune("89abAB", rune(id[19])) {
		return ""
	}
	return id
}
