package redact

import "testing"

func TestURLRedactsSecrets(t *testing.T) {
	got := URL("https://service.ipzan.com/core-extract?num=1&no=abc&secret=def")
	if got == "https://service.ipzan.com/core-extract?num=1&no=abc&secret=def" {
		t.Fatal("URL was not redacted")
	}
	if containsAny(got, "abc", "def") {
		t.Fatalf("redacted URL leaked secret: %s", got)
	}
}

func containsAny(s string, values ...string) bool {
	for _, value := range values {
		if value != "" && len(s) >= len(value) {
			for i := 0; i+len(value) <= len(s); i++ {
				if s[i:i+len(value)] == value {
					return true
				}
			}
		}
	}
	return false
}
