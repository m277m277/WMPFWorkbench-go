// Command wmpf-gui is the Go desktop presentation layer. Its pages mirror the
// original PySide6 console (overview / accounts / tools / search / logs / about)
// and share core.Service with MCP. Credentials stay in Go: the frontend only
// receives masked account data and session state.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"wmpfworkbench/contract"
	"wmpfworkbench/internal/app"
	"wmpfworkbench/internal/bundle"
	"wmpfworkbench/internal/config"
	"wmpfworkbench/internal/core"
	"wmpfworkbench/internal/hostruntime"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/app_icon.jpg
var iconData []byte

const appVersion = "1.0.0"

type Backend struct {
	ctx     context.Context
	svc     *core.Service
	cfgPath string
	mu      sync.Mutex
	logs    []string
}

func (b *Backend) Startup(ctx context.Context) {
	b.ctx = ctx
	// The HTTP MCP endpoint in the clipboard config must actually listen, or
	// clients report "fetch failed"; the GUI therefore owns its lifecycle.
	if values, err := b.svc.Config.Read(); err == nil && values["mcp_enabled"] != false {
		go func() {
			if err := runMCP(b.svc.Paths.Root, "http", 0, b.cfgPath); err != nil {
				b.logf("MCP HTTP 服务未启动：%v（若端口被占用，说明已有实例在运行）", err)
				return
			}
			b.logf("MCP HTTP 服务已退出")
		}()
	}
	go func() {
		status, err := b.svc.LoginSvc.Ensure(ctx)
		if err != nil {
			b.logf("登录服务启动失败：%s", status.Error)
			return
		}
		b.logf("登录服务已自动启动（%s）", status.URL)
	}()
}

// MCPStatus reports whether something is listening on the configured MCP HTTP
// port, so the UI shows real state instead of assuming availability.
func (b *Backend) MCPStatus() map[string]any {
	values, err := b.svc.Config.Read()
	port := 0
	enabled := true
	if err == nil {
		port = config.Int(values, "mcp_port")
		enabled = values["mcp_enabled"] != false
	}
	if port < 1 || port > 65535 {
		port = 18767
	}
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	running := dialErr == nil
	if conn != nil {
		conn.Close()
	}
	return map[string]any{"running": running, "enabled": enabled, "port": port, "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", port)}
}

var (
	openidPattern = regexp.MustCompile(`ow[A-Za-z0-9_-]{10,}`)
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]+`)
)

// maskLog keeps account identifiers and tokens out of the visible operation log.
func maskLog(text string) string {
	text = openidPattern.ReplaceAllStringFunc(text, func(match string) string { return match[:6] + "…" })
	return bearerPattern.ReplaceAllString(text, "Bearer ***")
}

func (b *Backend) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, time.Now().Format("15:04:05")+"   "+maskLog(fmt.Sprintf(format, args...)))
	if len(b.logs) > 400 {
		b.logs = b.logs[len(b.logs)-400:]
	}
}

type ToolView struct {
	Name        string          `json:"name"`
	Group       string          `json:"group"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

func toolGroup(name string) string {
	switch {
	case strings.HasPrefix(name, "search_"), strings.HasPrefix(name, "get_app"), name == "index":
		return "搜索与索引"
	case name == "login_status":
		return "账号"
	case name == "launch", name == "close_runtime", name == "runtime_status":
		return "运行时"
	case strings.HasPrefix(name, "cdp_"):
		return "调试"
	case name == "decrypt_wxapkg", name == "extract_api_surface":
		return "源码"
	case name == "netlog":
		return "网络"
	}
	return "其他"
}

func (b *Backend) Bootstrap() map[string]any {
	return map[string]any{
		"root":     b.svc.Paths.Root,
		"version":  appVersion,
		"sessions": b.svc.Sessions(),
		"tools":    b.Tools(),
		"config":   b.Config(),
		"logs":     b.Logs(),
	}
}

func (b *Backend) Config() map[string]any {
	values, err := b.svc.Config.Read()
	if err != nil {
		return map[string]any{}
	}
	hide, _ := values["hide_miniapp_window"].(bool)
	enabled, _ := values["mcp_enabled"].(bool)
	login, _ := values["login_port"].(float64)
	mcpPort, _ := values["mcp_port"].(float64)
	selected, _ := values["selected_account"].(string)
	return map[string]any{
		"hide_miniapp_window": hide,
		"mcp_enabled":         enabled,
		"login_port":          int(login),
		"mcp_port":            int(mcpPort),
		"selected_account":    selected,
	}
}

func (b *Backend) SavePorts(loginPort, mcpPort int) (map[string]any, error) {
	if loginPort < 1024 || loginPort > 65535 || mcpPort < 1024 || mcpPort > 65535 {
		return nil, fmt.Errorf("端口范围应为 1024–65535")
	}
	// Stop with the old settings first: after the update the URL points at the
	// new port and the old service would no longer be reachable.
	status := b.svc.LoginSvc.Status(b.ctx)
	if status.Running {
		b.svc.LoginSvc.Stop(b.ctx)
		b.logf("登录服务已停止，准备应用新端口")
	}
	if _, err := b.svc.Config.Update(b.ctx, map[string]any{"login_port": float64(loginPort), "mcp_port": float64(mcpPort)}); err != nil {
		return nil, err
	}
	b.logf("端口已保存：登录 %d / MCP %d", loginPort, mcpPort)
	if status.Running {
		if _, err := b.svc.LoginSvc.Ensure(b.ctx); err != nil {
			b.logf("登录服务按新端口重启失败：%v", err)
			return b.Config(), nil
		}
		b.logf("登录服务已按新端口重启")
	}
	return b.Config(), nil
}

// LoginStatus returns masked account information plus service state.
func (b *Backend) LoginStatus() (any, error) {
	return b.svc.Call(b.ctx, "login_status", nil)
}

func (b *Backend) StartLogin() (any, error) {
	status, err := b.svc.LoginSvc.Ensure(b.ctx)
	if err != nil {
		b.logf("登录服务启动失败：%s", status.Error)
		return status, err
	}
	b.logf("登录服务已就绪")
	return status, nil
}

func (b *Backend) StopLogin() any {
	status := b.svc.LoginSvc.Stop(b.ctx)
	b.logf("登录服务已停止")
	return status
}

func (b *Backend) RefreshAccount() (any, error) {
	values, err := b.svc.Config.Read()
	if err != nil {
		return nil, err
	}
	ref, _ := values["selected_account"].(string)
	if ref == "" {
		return map[string]any{"ok": false, "reason": "请先扫码登录并选择账号"}, nil
	}
	result, err := b.svc.Account.Request(b.ctx, "/accounts/refresh", "POST", map[string]any{"ref": ref})
	if err != nil {
		return nil, err
	}
	b.logf("账号检查完成")
	return result, nil
}

func (b *Backend) SelectAccount(ref string) error {
	if _, err := b.svc.Config.Update(b.ctx, map[string]any{"selected_account": ref}); err != nil {
		return err
	}
	b.logf("已切换当前账号")
	return nil
}

// Avatar returns a data URI for the account's profile image. The image is
// fetched by Go from the local login service; no token reaches the frontend.
func (b *Backend) Avatar(ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	raw, err := b.svc.Account.Avatar(b.ctx, ref)
	if err != nil || len(raw) == 0 {
		return "", err
	}
	return "data:" + http.DetectContentType(raw) + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

func (b *Backend) QRCreate() (any, error) {
	result, err := b.svc.LoginSvc.QRCreate(b.ctx)
	if err != nil {
		b.logf("二维码生成失败")
		return nil, err
	}
	b.logf("新二维码已生成，等待微信扫描")
	return result, nil
}

func (b *Backend) QRPoll(session string) (any, error) {
	return b.svc.LoginSvc.QRPoll(b.ctx, session)
}

func (b *Backend) QRConfirm(session string) (any, error) {
	value, err := b.svc.LoginSvc.QRConfirm(b.ctx, session)
	if err != nil {
		return nil, err
	}
	if account, ok := value.(map[string]any); ok {
		if openid, ok := account["openid"].(string); ok && openid != "" {
			b.svc.Config.Update(b.ctx, map[string]any{"selected_account": openid})
		}
	}
	b.logf("扫码登录完成，账号已保存并设为当前账号")
	return value, nil
}

func (b *Backend) Search(keyword, source string) (any, error) {
	if strings.TrimSpace(keyword) == "" {
		return map[string]any{"items": []any{}, "hits": 0}, nil
	}
	if source != "local" {
		source = "online"
	}
	b.logf("搜索：%s（%s）", keyword, source)
	return b.svc.Call(b.ctx, "search_mini_programs", map[string]any{"keyword": keyword, "source": source, "limit": 30})
}

func (b *Backend) Launch(appid, entry string, seconds int, hide bool) (any, error) {
	if _, err := b.svc.Config.Update(b.ctx, map[string]any{"hide_miniapp_window": hide}); err != nil {
		return nil, err
	}
	b.logf("打开并调试：%s", appid)
	result, err := b.svc.Call(b.ctx, "launch", map[string]any{"appid": appid, "path": entry, "seconds": float64(seconds), "hide_window": hide})
	if err != nil {
		b.logf("启动失败：%v", err)
		return nil, err
	}
	if row, ok := result.(map[string]any); ok {
		if fmt.Sprint(row["state"]) == "error" {
			b.logf("会话启动失败：%v", row["error"])
		} else {
			b.logf("会话已创建：%v", row["state"])
		}
	}
	return result, nil
}

func (b *Backend) Close(session string) (any, error) {
	result, err := b.svc.Call(b.ctx, "close_runtime", map[string]any{"session_id": session})
	if err == nil {
		b.logf("已关闭会话 %s", session)
	}
	return result, err
}

func (b *Backend) Sessions() []map[string]any { return b.svc.Sessions() }

func (b *Backend) Tools() []ToolView {
	tools, _, err := contract.Catalog()
	if err != nil {
		return []ToolView{}
	}
	out := make([]ToolView, 0, len(tools))
	for _, tool := range tools {
		out = append(out, ToolView{Name: tool.Name, Group: toolGroup(tool.Name), Description: tool.Description, Enabled: b.svc.Config.Enabled(tool.Name), Schema: tool.InputSchema})
	}
	return out
}

func (b *Backend) SetTool(name string, enabled bool) ([]ToolView, error) {
	return b.BulkTools([]string{name}, enabled)
}

func (b *Backend) BulkTools(names []string, enabled bool) ([]ToolView, error) {
	values, err := b.svc.Config.Read()
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, name := range names {
		targets[name] = true
	}
	disabled := []any{}
	if list, ok := values["disabled_tools"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok && !targets[text] {
				disabled = append(disabled, text)
			}
		}
	}
	if !enabled {
		for name := range targets {
			disabled = append(disabled, name)
		}
	}
	if _, err = b.svc.Config.Update(b.ctx, map[string]any{"disabled_tools": disabled}); err != nil {
		return nil, err
	}
	b.logf("已%s %d 个工具", map[bool]string{true: "启用", false: "禁用"}[enabled], len(names))
	return b.Tools(), nil
}

func (b *Backend) GlobalTools(enabled bool) (map[string]any, error) {
	if _, err := b.svc.Config.Update(b.ctx, map[string]any{"mcp_enabled": enabled}); err != nil {
		return nil, err
	}
	b.logf("MCP 总开关：%v", enabled)
	return b.Config(), nil
}

// Icon returns the original project icon for the About page, so the UI does
// not carry a hand-drawn placeholder.
func (b *Backend) Icon() string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(iconData)
}

// OpenURL opens an external documentation link in the system browser.
func (b *Backend) OpenURL(target string) {
	if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
		runtime.BrowserOpenURL(b.ctx, target)
	}
}

// mcpConfig mirrors the original console's stdio/http client entries: stdio
// carries type/command/args/env/description, http carries type/url/headers.
func mcpConfig(kind, exe, root string, values map[string]any) (map[string]any, error) {
	switch kind {
	case "stdio":
		return map[string]any{
			"type":    "stdio",
			"command": exe,
			"args":    []string{"-mcp", "stdio", "-root", root},
			"env": map[string]string{
				"WMPF_HOME":      root,
				"WMPF_AUTH_MODE": "managed",
			},
			"description": "WMPF Workbench（stdio，与 GUI 共享账号与开关）",
		}, nil
	case "http":
		return map[string]any{
			"type":        "streamable-http",
			"url":         fmt.Sprintf("http://127.0.0.1:%d/mcp", config.Int(values, "mcp_port")),
			"headers":     map[string]string{"Authorization": "Bearer " + config.String(values, "mcp_token")},
			"description": "WMPF Workbench（HTTP，与 GUI 共用运行会话）",
		}, nil
	default:
		return nil, fmt.Errorf("未知配置类型")
	}
}

// CopyMCPConfig puts a ready-to-paste MCP client configuration on the
// clipboard, matching the original console's stdio/http entries.
func (b *Backend) CopyMCPConfig(kind string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	values, err := b.svc.Config.Read()
	if err != nil {
		return err
	}
	entry, err := mcpConfig(kind, exe, b.svc.Paths.Root, values)
	if err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"mcpServers": map[string]any{"wmpf": entry}}); err != nil {
		return err
	}
	runtime.ClipboardSetText(b.ctx, buffer.String())
	b.logf("已复制 %s MCP 配置", kind)
	return nil
}

func (b *Backend) Logs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.logs))
	copy(out, b.logs)
	return out
}

func (b *Backend) ClearLogs() []string {
	b.mu.Lock()
	b.logs = nil
	b.mu.Unlock()
	return []string{}
}

func main() {
	root := flag.String("root", os.Getenv("WMPF_HOME"), "installation root")
	runtimeDir := flag.String("runtime", "", "override runtime directory (default <root>/runtime/5.10.7300.414)")
	cfgPath := flag.String("config", "", "override console configuration path")
	selfCheck := flag.Bool("selfcheck", false, "create the window, then exit")
	mcpMode := flag.String("mcp", "", "run as MCP server: stdio or http")
	mcpPort := flag.Int("mcp-port", 0, "MCP HTTP port (default from config)")
	transport := flag.String("transport", "", "MCP transport alias: stdio or streamable-http")
	port := flag.Int("port", 0, "MCP HTTP port alias")
	flag.Parse()
	if *runtimeDir != "" {
		os.Setenv("WMPF_RUNTIME_DIR", *runtimeDir)
	}
	mode := *mcpMode
	if mode == "" && *transport != "" {
		switch *transport {
		case "stdio":
			mode = "stdio"
		case "streamable-http":
			mode = "http"
		}
	}
	if mode != "" {
		chosen := *mcpPort
		if chosen == 0 {
			chosen = *port
		}
		if err := runMCP(*root, mode, chosen, *cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	p, err := app.Resolve(*root)
	if err != nil {
		os.Exit(1)
	}
	if helpers, err := bundle.Ensure(p.Data); err == nil {
		bundle.Apply(helpers)
	}
	svc := core.New(p)
	if *cfgPath != "" {
		svc.Config = config.New(*cfgPath)
		svc.Account.Config = svc.Config
		svc.Runtime = hostruntime.New(p, svc.Config, svc.Account)
	}
	defer svc.Close()
	backend := &Backend{svc: svc, cfgPath: *cfgPath}
	err = wails.Run(&options.App{
		Title:     "小程序工作台",
		Width:     920,
		Height:    620,
		MinWidth:  560,
		MinHeight: 420,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: backend.Startup,
		Bind:      []any{backend},
		OnDomReady: func(ctx context.Context) {
			exe, _ := os.Executable()
			go func() {
				time.Sleep(200 * time.Millisecond)
				applyWindowIcon("小程序工作台", exe)
			}()
			if *selfCheck {
				go func() {
					time.Sleep(700 * time.Millisecond)
					runtime.Quit(ctx)
				}()
			}
		},
	})
	if err != nil {
		os.Exit(1)
	}
}
