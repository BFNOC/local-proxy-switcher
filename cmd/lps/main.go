package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/config"
	"github.com/BFNOC/local-proxy-switcher/internal/control"
	"github.com/BFNOC/local-proxy-switcher/internal/mixed"
	"github.com/BFNOC/local-proxy-switcher/internal/provider"
	"github.com/BFNOC/local-proxy-switcher/internal/refresher"
	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "status":
		return status(args[1:])
	case "switch":
		return switchProxy(args[1:])
	case "lock":
		return lock(args[1:])
	case "clear":
		return clear(args[1:])
	case "open":
		return openUI(args[1:])
	case "version":
		return printVersion(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func usage() {
	fmt.Print(`local-proxy-switcher

用法:
  lps serve [--config config.yaml] [--upstream http://host:port]
  lps status [--control 127.0.0.1:17990]
  lps switch [--control 127.0.0.1:17990] [--interrupt]
  lps lock [--control 127.0.0.1:17990] [--interrupt] <proxy-url>
  lps clear [--control 127.0.0.1:17990] [--interrupt]
  lps open [--control 127.0.0.1:17990]
  lps version

`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultLocalPath(config.DefaultConfigFileName), "配置文件路径")
	initialUpstream := fs.String("upstream", "", "启动时锁定的上游代理地址")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *initialUpstream != "" {
		cfg.Upstream.URL = *initialUpstream
	}
	if !config.IsLoopbackBind(cfg.Listen.Control) {
		return fmt.Errorf("control bind must be loopback in this version: %s", cfg.Listen.Control)
	}

	tr := tracker.New()
	sel := selector.New(selector.Options{
		Tracker:             tr,
		DialTimeout:         cfg.Upstream.DialTimeoutDuration(),
		FailClosedOnExpired: cfg.Switching.FailClosedOnExpired,
		FallbackToDirect:    cfg.Switching.FallbackToDirect,
		HistoryLimit:        cfg.Switching.HistoryLimit,
		InterruptByDefault:  cfg.Switching.InterruptExistingDefault,
	})

	if cfg.Upstream.URL != "" {
		u, err := upstream.Parse(cfg.Upstream.URL, cfg.Provider.DefaultScheme, cfg.Provider.DefaultTTLDuration())
		if err != nil {
			return fmt.Errorf("解析启动上游失败: %w", err)
		}
		u.Source = "config"
		sel.Switch(u, false, "initial config")
	}

	var fetcher *provider.Fetcher
	if cfg.Provider.Enabled && cfg.Provider.URL != "" {
		fetcher = provider.NewFetcher(provider.Options{
			URL:           cfg.Provider.URL,
			Timeout:       cfg.Provider.TimeoutDuration(),
			DefaultScheme: cfg.Provider.DefaultScheme,
			DefaultTTL:    cfg.Provider.DefaultTTLDuration(),
		})
	}
	if cfg.Switching.AutoRefresh && fetcher == nil {
		return errors.New("auto_refresh requires provider.enabled and provider.url")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Switching.AutoRefresh {
		auto := refresher.New(refresher.Options{
			Fetcher:   fetcher,
			Selector:  sel,
			Interrupt: cfg.Switching.InterruptExistingDefault,
		})
		go auto.Run(ctx)
	}

	ctrl := control.NewServer(control.Options{
		Addr:      cfg.Listen.Control,
		Selector:  sel,
		Tracker:   tr,
		Fetcher:   fetcher,
		ManualTTL: cfg.Provider.DefaultTTLDuration(),
	})
	mix := mixed.NewServer(mixed.Options{
		Addr:             cfg.Listen.Mixed,
		Selector:         sel,
		Tracker:          tr,
		HandshakeTimeout: cfg.Upstream.DialTimeoutDuration(),
	})

	errCh := make(chan error, 2)
	go func() { errCh <- ctrl.ListenAndServe(ctx) }()
	go func() { errCh <- mix.ListenAndServe(ctx) }()

	fmt.Printf("mixed listening:   %s\n", cfg.Listen.Mixed)
	fmt.Printf("控制面板:          http://%s\n", cfg.Listen.Control)
	if cfg.Switching.AutoRefresh {
		fmt.Println("自动刷新:          已启用")
	}
	if cur, ok := sel.Current(); ok {
		fmt.Printf("当前上游:          %s\n", cur.RedactedURL())
	} else {
		fmt.Println("当前上游:          无")
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tr.Interrupt()
		return ctrl.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	controlAddr := fs.String("control", config.DefaultControlAddr, "控制 API 地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var st control.StatusResponse
	if err := controlRequest(context.Background(), http.MethodGet, *controlAddr, "/status", nil, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func switchProxy(args []string) error {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	controlAddr := fs.String("control", config.DefaultControlAddr, "控制 API 地址")
	interrupt := fs.Bool("interrupt", false, "中断已有连接")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/switch"
	if *interrupt {
		path += "?interrupt=true"
	}
	var st control.StatusResponse
	if err := controlRequest(context.Background(), http.MethodPost, *controlAddr, path, nil, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func lock(args []string) error {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	controlAddr := fs.String("control", config.DefaultControlAddr, "控制 API 地址")
	interrupt := fs.Bool("interrupt", false, "中断已有连接")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("lock requires one proxy URL")
	}
	body, _ := json.Marshal(map[string]string{"url": fs.Arg(0)})
	path := "/lock"
	if *interrupt {
		path += "?interrupt=true"
	}
	var st control.StatusResponse
	if err := controlRequest(context.Background(), http.MethodPost, *controlAddr, path, body, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func clear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	controlAddr := fs.String("control", config.DefaultControlAddr, "控制 API 地址")
	interrupt := fs.Bool("interrupt", false, "中断已有连接")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/clear"
	if *interrupt {
		path += "?interrupt=true"
	}
	var st control.StatusResponse
	if err := controlRequest(context.Background(), http.MethodPost, *controlAddr, path, nil, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func openUI(args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	controlAddr := fs.String("control", config.DefaultControlAddr, "控制 API 地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := "http://" + *controlAddr + "/ui"
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println(u)
		return err
	}
	fmt.Println(u)
	return nil
}

var version = "dev"

func printVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Println(version)
	return nil
}

func controlRequest(ctx context.Context, method, addr, path string, body []byte, out any) error {
	u := "http://" + addr + path
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func printStatus(st control.StatusResponse) {
	if st.Current == nil {
		fmt.Println("当前上游: 无")
	} else {
		fmt.Println("当前上游:", st.Current.URL)
		if st.Current.Net != "" {
			fmt.Println("线路:", st.Current.Net)
		}
		fmt.Println("剩余时间:", st.Current.ExpiresIn)
	}
	fmt.Println("活跃连接:", st.ActiveConnections)
	if st.LastSwitch != "" {
		fmt.Println("最近切换:", st.LastSwitch)
	}
	if st.LastError != "" {
		fmt.Println("最近错误:", st.LastError)
	}
}
