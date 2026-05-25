package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultLocalPathUsesExecutableDir(t *testing.T) {
	got := DefaultLocalPath(DefaultConfigFileName)
	if filepath.Base(got) != DefaultConfigFileName {
		t.Fatalf("base = %q, want %s", filepath.Base(got), DefaultConfigFileName)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("path is not absolute: %s", got)
	}
}
