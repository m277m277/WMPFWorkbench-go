// Package loginsvc owns the lifecycle of the separate Go login service process.
// It keeps credentials in the service and never logs the desktop token.
package loginsvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"wmpfworkbench/internal/app"
	"wmpfworkbench/internal/config"
	"wmpfworkbench/internal/process"
	"yyb_go/embedded"
)

type Status struct {
	Running bool   `json:"running"`
	Healthy bool   `json:"healthy"`
	Owned   bool   `json:"owned"`
	PID     int    `json:"pid,omitempty"`
	URL     string `json:"url"`
	Binary  string `json:"binary"`
	Port    int    `json:"port"`
	Error   string `json:"error,omitempty"`
}

type Manager struct {
	Paths   app.Paths
	Config  *config.Store
	HTTP    *http.Client
	Command func(name string, args []string, env []string) *exec.Cmd

	mu   sync.Mutex
	proc *exec.Cmd
	done chan struct{}

	embeddedService *embedded.Service
}

func New(p app.Paths, cfg *config.Store) *Manager {
	return &Manager{
		Paths:  p,
		Config: cfg,
		HTTP:   &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second},
		Command: func(name string, args []string, env []string) *exec.Cmd {
			cmd := exec.Command(name, args...)
			cmd.Env = env
			cmd.Dir = p.Root
			process.Hide(cmd)
			return cmd
		},
	}
}

// Binary is the login service executable. A bundled copy injected through
// WMPF_LOGIN_BIN wins over the original project's desktop/bin layout.
func (m *Manager) Binary() string {
	if value := os.Getenv("WMPF_LOGIN_BIN"); value != "" {
		return value
	}
	return filepath.Join(m.Paths.Root, "desktop", "bin", "yyb-login-service.exe")
}

type settings struct {
	URL         string
	Port        int
	Token       string
	ResourceDir string
}

func (m *Manager) settings() (settings, error) {
	values, err := m.Config.Read()
	if err != nil {
		return settings{}, err
	}
	port, _ := values["login_port"].(float64)
	if port == 0 {
		port = 18766
	}
	s := settings{Port: int(port), ResourceDir: filepath.Join(m.Paths.Data, "desktop", "login")}
	s.URL = config.String(values, "login_url")
	if s.URL == "" {
		s.URL = fmt.Sprintf("http://127.0.0.1:%d", s.Port)
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return s, errors.New("登录服务地址必须是本机 http://127.0.0.1")
	}
	s.Token = config.String(values, "login_token")
	if s.Token == "" {
		var raw [32]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return s, err
		}
		s.Token = base64.RawURLEncoding.EncodeToString(raw[:])
		if _, err = m.Config.Update(context.Background(), map[string]any{"login_token": s.Token}); err != nil {
			return s, err
		}
	}
	return s, nil
}

// Request performs an authenticated desktop request and unwraps the service
// envelope. Callers never see the desktop token.
func (m *Manager) Request(ctx context.Context, path, method string, body any) (any, error) {
	s, err := m.settings()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.URL+path, reader)
	if err != nil {
		return nil, errors.New("登录服务请求无效")
	}
	request.Header.Set("Authorization", "Bearer "+s.Token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 65 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("登录服务连接失败或请求超时")
	}
	defer response.Body.Close()
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&envelope) != nil || envelope.Code != 0 || envelope.Data == nil {
		return nil, fmt.Errorf("登录服务返回无效数据（HTTP %d）", response.StatusCode)
	}
	var result any
	if json.Unmarshal(envelope.Data, &result) != nil {
		return nil, errors.New("登录服务返回无效数据")
	}
	return result, nil
}

func (m *Manager) QRCreate(ctx context.Context) (any, error) {
	return m.Request(ctx, "/qr?as_base64=true", "POST", map[string]any{})
}

func (m *Manager) QRPoll(ctx context.Context, session string) (any, error) {
	return m.Request(ctx, "/qr/"+url.PathEscape(session)+"/poll", "GET", nil)
}

func (m *Manager) QRConfirm(ctx context.Context, session string) (any, error) {
	return m.Request(ctx, "/qr/"+url.PathEscape(session)+"/confirm", "POST", map[string]any{})
}

func (m *Manager) healthy(ctx context.Context, s settings) bool {
	request, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/health", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+s.Token)
	response, err := m.HTTP.Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == 200
}

// Ensure returns a healthy service, starting the owned process when the port is
// free. An already running external service is reused but never killed by us.
func (m *Manager) Ensure(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.settings()
	if err != nil {
		return Status{URL: s.URL, Binary: m.Binary(), Port: s.Port, Error: err.Error()}, err
	}
	status := Status{URL: s.URL, Binary: m.Binary(), Port: s.Port}
	if m.healthy(ctx, s) {
		status.Running, status.Healthy = true, true
		if m.proc != nil && m.proc.Process != nil {
			status.Owned, status.PID = true, m.proc.Process.Pid
		}
		return status, nil
	}
	// The login service runs inside this process; no helper binary is released.
	if m.embeddedService != nil {
		if m.healthy(ctx, s) {
			status.Running, status.Healthy, status.Owned, status.PID = true, true, true, os.Getpid()
			return status, nil
		}
		m.embeddedService.Stop(context.Background())
		m.embeddedService = nil
	}
	os.MkdirAll(s.ResourceDir, 0700)
	service, err := embedded.Start(s.ResourceDir, s.Port, s.Token)
	if err != nil {
		status.Error = "登录服务启动失败：" + err.Error()
		return status, errors.New(status.Error)
	}
	m.embeddedService = service
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthy(ctx, s) {
			status.Running, status.Healthy, status.Owned, status.PID = true, true, true, os.Getpid()
			return status, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(150 * time.Millisecond):
		}
	}
	m.embeddedService.Stop(context.Background())
	m.embeddedService = nil
	status.Error = "登录服务健康检查超时"
	return status, errors.New(status.Error)
}

// Stop requests the graceful shutdown endpoint, then terminates only a process
// this manager started.
func (m *Manager) Stop(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.settings()
	status := Status{URL: s.URL, Binary: m.Binary(), Port: s.Port}
	if err != nil {
		status.Error = err.Error()
		return status
	}
	if m.embeddedService != nil {
		m.embeddedService.Stop(ctx)
		m.embeddedService = nil
		status.Running, status.Healthy = false, false
		return status
	}
	if m.healthy(ctx, s) {
		request, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/internal/shutdown", strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer "+s.Token)
		request.Header.Set("Content-Type", "application/json")
		if response, err := m.HTTP.Do(request); err == nil {
			response.Body.Close()
		}
	}
	if m.proc == nil {
		status.Running = m.healthy(ctx, s)
		status.Healthy = status.Running
		return status
	}
	status.Running, status.Owned, status.PID = true, true, m.proc.Process.Pid
	select {
	case <-m.done:
	case <-ctx.Done():
	case <-time.After(25 * time.Second):
	}
	if m.proc != nil && m.proc.Process != nil {
		select {
		case <-m.done:
		default:
			m.proc.Process.Kill()
			select {
			case <-m.done:
			case <-time.After(5 * time.Second):
			}
		}
	}
	m.proc, m.done = nil, nil
	status.Running = false
	return status
}

// Status reports the current state without starting or stopping anything.
func (m *Manager) Status(ctx context.Context) Status {
	s, err := m.settings()
	status := Status{URL: s.URL, Binary: m.Binary(), Port: s.Port}
	if err != nil {
		status.Error = err.Error()
		return status
	}
	if m.healthy(ctx, s) {
		status.Running, status.Healthy = true, true
		m.mu.Lock()
		if m.embeddedService != nil {
			status.Owned, status.PID = true, os.Getpid()
		} else if m.proc != nil && m.proc.Process != nil {
			status.Owned, status.PID = true, m.proc.Process.Pid
		}
		m.mu.Unlock()
	}
	return status
}
