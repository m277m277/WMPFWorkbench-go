package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"wmpfworkbench/internal/config"
)

type Client struct {
	Config *config.Store
	HTTP   *http.Client
}
type Session struct {
	Cookie                          string
	Pairs                           map[string]string
	SDKToken, SDKScope, LoginBuffer string
}

func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func New(cfg *config.Store) *Client { return &Client{cfg, HTTPClient(95 * time.Second)} }

// base resolves the local login-service endpoint the same way loginsvc does:
// an empty login_url falls back to http://127.0.0.1:<login_port>. Keeping the
// two paths consistent avoids "登录服务尚未启动" on a first run whose defaults
// leave login_url blank while the service is actually up on the default port.
func (c *Client) base(values config.Values) (string, string, error) {
	base := config.String(values, "login_url")
	if base == "" {
		port := config.Int(values, "login_port")
		if port == 0 {
			port = 18766
		}
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return "", "", errors.New("登录服务地址必须是本机 http://127.0.0.1")
	}
	// Use the shared, lock-protected generator so the token cannot diverge from
	// the one the login service mints at startup (which caused 401s on first run).
	token, err := c.Config.EnsureToken(context.Background(), "login_token")
	if err != nil {
		return "", "", err
	}
	return base, token, nil
}

// Avatar returns the raw profile image. The desktop token only goes to the
// local login service, never to a remote host.
func (c *Client) Avatar(ctx context.Context, ref string) ([]byte, error) {
	values, err := c.Config.Read()
	if err != nil {
		return nil, err
	}
	base, token, err := c.base(values)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, "GET", base+"/accounts/avatar?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("头像获取失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("头像获取失败")
	}
	return io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
}

func (c *Client) Request(ctx context.Context, path, method string, body any) (any, error) {
	cfg, err := c.Config.Read()
	if err != nil {
		return nil, err
	}
	base, token, err := c.base(cfg)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("登录服务尚未启动")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, errors.New("登录服务地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("登录服务连接失败或请求超时")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("登录服务请求失败，HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&envelope) != nil || envelope.Code != 0 || envelope.Data == nil {
		return nil, errors.New("登录服务返回无效数据")
	}
	var result any
	if json.Unmarshal(envelope.Data, &result) != nil {
		return nil, errors.New("登录服务返回无效数据")
	}
	return result, nil
}
func Percent(value string) string {
	var b strings.Builder
	const alphabet = "0123456789ABCDEF"
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(alphabet[c>>4])
			b.WriteByte(alphabet[c&15])
		}
	}
	return b.String()
}
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func (c *Client) Session(ctx context.Context) (Session, error) {
	result := Session{}
	cfg, err := c.Config.Read()
	if err != nil {
		return result, err
	}
	ref := config.String(cfg, "selected_account")
	if ref == "" {
		return result, errors.New("请扫码登录并选择账号")
	}
	fetch := func() (map[string]any, map[string]any, error) {
		v, err := c.Request(ctx, "/internal/session?ref="+url.QueryEscape(ref), "GET", nil)
		if err != nil {
			return nil, nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("登录会话数据无效")
		}
		creds, ok := m["credentials"].(map[string]any)
		if !ok {
			return nil, nil, errors.New("登录凭据不存在")
		}
		return m, creds, nil
	}
	value, creds, err := fetch()
	if err != nil {
		return result, err
	}
	expiry, _ := creds["expires_at"].(float64)
	sdkExpiry, _ := creds["wxapp_access_token_expire_time"].(float64)
	// The service always serializes wxapp_access_token_expire_time, including 0
	// when it has never recorded an SDK expiry. Zero means "unknown", not expired;
	// treating it as expired refreshed on every launch.
	now := float64(time.Now().Unix())
	if expiry < now+60 || (sdkExpiry > 0 && sdkExpiry < now+60 && text(creds["wxapp_openid"]) != "") {
		refreshed, err := c.Request(ctx, "/accounts/refresh", "POST", map[string]any{"ref": ref})
		if err != nil {
			return result, err
		}
		state, _ := refreshed.(map[string]any)
		if state["status"] != "alive" {
			return result, errors.New("账号需要重新登录")
		}
		value, creds, err = fetch()
		if err != nil {
			return result, err
		}
	}
	pairs := map[string]string{}
	for _, part := range strings.Split(text(creds["cookie"]), ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			if _, exists := pairs[key]; !exists {
				decoded, err := url.PathUnescape(val)
				if err != nil {
					decoded = val
				}
				pairs[key] = decoded
			}
		}
	}
	for _, key := range []string{"openid", "accesstoken", "refreshtoken", "guid", "scope", "logintype", "wxapp_openid", "wxapp_accesstoken", "wxapp_refreshtoken", "wxapp_logintype", "wxapp_scope"} {
		if v, ok := creds[key]; ok {
			pairs[key] = text(v)
		}
	}
	if pairs["guid"] == "" || pairs["openid"] == "" || pairs["accesstoken"] == "" {
		return result, errors.New("登录凭据不完整")
	}
	chunks := []string{}
	for key, val := range pairs {
		if val != "" {
			chunks = append(chunks, key+"="+Percent(val))
		}
	}
	result = Session{Cookie: strings.Join(chunks, "; "), Pairs: pairs, SDKToken: pairs["accesstoken"], SDKScope: pairs["scope"], LoginBuffer: text(value["login_buffer"])}
	if pairs["wxapp_logintype"] == "WX" && pairs["wxapp_openid"] != "" {
		result.SDKToken = pairs["wxapp_accesstoken"]
		result.SDKScope = pairs["wxapp_scope"]
	}
	return result, nil
}
