package config

import (
	"os"
	"path/filepath"
)

// DefaultConfigFileName 是不传 --config 时读取的配置文件名。
const DefaultConfigFileName = "config.yaml"

// DefaultLocalPath 返回可执行文件同目录下的默认配置路径。
func DefaultLocalPath(name string) string {
	if name == "" {
		name = DefaultConfigFileName
	}
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	dir := filepath.Dir(exe)
	if dir == "." || dir == "" {
		return name
	}
	return filepath.Join(dir, name)
}
