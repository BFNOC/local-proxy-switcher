package redact

import (
	"net/url"
	"strings"
)

// URL 返回隐藏常见密钥参数和代理密码后的展示用地址。
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactKnownKeys(raw)
	}
	if u.User != nil {
		if username := u.User.Username(); username != "" {
			u.User = url.UserPassword(username, "***")
		}
	}
	q := u.Query()
	for _, key := range []string{"secret", "no", "token", "password", "pass"} {
		if _, ok := q[key]; ok {
			q.Set(key, "***")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func redactKnownKeys(value string) string {
	out := value
	for _, key := range []string{"secret=", "no=", "token=", "password=", "pass="} {
		idx := strings.Index(strings.ToLower(out), key)
		if idx < 0 {
			continue
		}
		start := idx + len(key)
		end := start
		for end < len(out) && out[end] != '&' && out[end] != ' ' {
			end++
		}
		out = out[:start] + "***" + out[end:]
	}
	return out
}
