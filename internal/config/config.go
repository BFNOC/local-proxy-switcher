package config

import (
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultMixedAddr 是本地 mixed 代理监听地址。
	DefaultMixedAddr = "127.0.0.1:7890"
	// DefaultControlAddr 是本地控制 API 监听地址。
	DefaultControlAddr = "127.0.0.1:17990"
)

// Config 是守护进程的完整配置。
type Config struct {
	Listen    ListenConfig    `yaml:"listen"`
	Provider  ProviderConfig  `yaml:"provider"`
	Upstream  UpstreamConfig  `yaml:"upstream"`
	Switching SwitchingConfig `yaml:"switching"`
	Logging   LoggingConfig   `yaml:"logging"`
}

// ListenConfig 定义本地 mixed 端口和控制 API 绑定地址。
type ListenConfig struct {
	Mixed   string `yaml:"mixed"`
	Control string `yaml:"control"`
}

// ProviderConfig 定义如何获取短效上游代理。
type ProviderConfig struct {
	Enabled       bool   `yaml:"enabled"`
	URL           string `yaml:"url"`
	Timeout       string `yaml:"timeout"`
	DefaultScheme string `yaml:"default_scheme"`
	DefaultTTL    string `yaml:"default_ttl"`
}

// UpstreamConfig 定义可选启动上游和出站超时。
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	DialTimeout string `yaml:"dial_timeout"`
}

// SwitchingConfig 控制切换行为和历史保留数量。
type SwitchingConfig struct {
	AutoRefresh              bool `yaml:"auto_refresh"`
	FailClosedOnExpired      bool `yaml:"fail_closed_on_expired"`
	InterruptExistingDefault bool `yaml:"interrupt_existing_default"`
	FallbackToDirect         bool `yaml:"fallback_to_direct"`
	HistoryLimit             int  `yaml:"history_limit"`
}

// LoggingConfig 控制进程日志。
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Default 返回安全的仅回环默认配置。
func Default() Config {
	return Config{
		Listen: ListenConfig{
			Mixed:   DefaultMixedAddr,
			Control: DefaultControlAddr,
		},
		Provider: ProviderConfig{
			Enabled:       true,
			Timeout:       "5s",
			DefaultScheme: "http",
			DefaultTTL:    "10m",
		},
		Upstream: UpstreamConfig{
			DialTimeout: "3s",
		},
		Switching: SwitchingConfig{
			FailClosedOnExpired: true,
			HistoryLimit:        50,
		},
		Logging: LoggingConfig{Level: "info"},
	}
}

// Load 读取 YAML 配置，展开环境变量并补齐默认值。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	if _, err := os.Stat(path); err != nil {
		return Config{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	expanded := os.ExpandEnv(string(content))
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return Config{}, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen.Mixed == "" {
		c.Listen.Mixed = DefaultMixedAddr
	}
	if c.Listen.Control == "" {
		c.Listen.Control = DefaultControlAddr
	}
	if c.Provider.Timeout == "" {
		c.Provider.Timeout = "5s"
	}
	if c.Provider.DefaultScheme == "" {
		c.Provider.DefaultScheme = "http"
	}
	if c.Provider.DefaultTTL == "" {
		c.Provider.DefaultTTL = "10m"
	}
	if c.Upstream.DialTimeout == "" {
		c.Upstream.DialTimeout = "3s"
	}
	if c.Switching.HistoryLimit <= 0 {
		c.Switching.HistoryLimit = 50
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
}

// TimeoutDuration 返回 provider 拉取超时，配置错误时使用安全默认值。
func (c ProviderConfig) TimeoutDuration() time.Duration {
	return parseDuration(c.Timeout, 5*time.Second)
}

// DefaultTTLDuration 返回 provider 普通文本响应使用的默认有效期。
func (c ProviderConfig) DefaultTTLDuration() time.Duration {
	return parseDuration(c.DefaultTTL, 10*time.Minute)
}

// DialTimeoutDuration 返回连接上游和目标时使用的拨号超时。
func (c UpstreamConfig) DialTimeoutDuration() time.Duration {
	return parseDuration(c.DialTimeout, 3*time.Second)
}

func parseDuration(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}

// IsLoopbackBind 判断地址是否绑定在 localhost 或回环 IP。
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
