package goagent

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxRequestBody 是 HTTP 请求体的上限（含图片的消息也在此范围内）。
const maxRequestBody = 32 << 20

// httpSecurity 是 RunHTTP 的请求准入层：鉴权、跨站请求拦截、DNS 重绑定
// 防护（只监听回环地址时校验 Host）与请求体上限。
type httpSecurity struct {
	token        string
	origins      map[string]bool // 允许跨源访问的源（scheme://host[:port]）
	hosts        map[string]bool // 允许的 Host（来自允许的源）
	loopbackOnly bool            // 服务只监听回环地址
}

func newHTTPSecurity(cfg appConfig, loopbackOnly bool) *httpSecurity {
	s := &httpSecurity{
		token:        cfg.httpAuthToken,
		origins:      make(map[string]bool),
		hosts:        make(map[string]bool),
		loopbackOnly: loopbackOnly,
	}
	for _, o := range cfg.httpAllowedOrigins {
		o = strings.TrimRight(o, "/")
		s.origins[o] = true
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			s.hosts[strings.ToLower(u.Hostname())] = true
		}
	}
	return s
}

func (s *httpSecurity) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		originAllowed := origin != "" && s.origins[strings.TrimRight(origin, "/")]
		if originAllowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-GoAgent-Token, Last-Event-ID")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		// 跨站请求（CSRF）：浏览器把来自其他站点的请求标记为 cross-site；
		// 不支持该头的旧浏览器退回比较 Origin 与 Host。
		if !originAllowed {
			site := r.Header.Get("Sec-Fetch-Site")
			if site == "cross-site" || (site == "" && origin != "" && !sameHost(origin, r.Host)) {
				http.Error(w, "跨站请求被拒绝：请用 WithHTTPAllowedOrigins 放行该来源", http.StatusForbidden)
				return
			}
		}

		// DNS 重绑定：恶意域名解析到 127.0.0.1 后，浏览器视其为同源。
		// 只监听回环地址时要求 Host 也是回环名（或允许的源）。
		if s.loopbackOnly {
			host := hostOnly(r.Host)
			if !isLoopbackHost(host) && !s.hosts[host] {
				http.Error(w, "Host 不被允许", http.StatusForbidden)
				return
			}
		}

		if s.token != "" && !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="goagent"`)
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}

		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *httpSecurity) authorized(r *http.Request) bool {
	got := ""
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		got = strings.TrimPrefix(a, "Bearer ")
	} else if t := r.Header.Get("X-GoAgent-Token"); t != "" {
		got = t
	} else {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// resolveListenAddr 按安全策略确定监听地址：未配置鉴权令牌时只允许回环
// 地址（":8080" 这类省略主机的写法收紧为 127.0.0.1），显式的非回环地址
// 直接报错——除非宿主用 WithHTTPInsecure 明确声明网络已受其他机制保护。
func resolveListenAddr(addr string, cfg appConfig) (resolved string, loopbackOnly bool, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, fmt.Errorf("goagent: 监听地址 %q 无效: %w", addr, err)
	}
	if cfg.httpAuthToken != "" || cfg.httpInsecure {
		return addr, host != "" && isLoopbackHost(host), nil
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopbackHost(host) {
		return "", false, fmt.Errorf("goagent: 拒绝在非回环地址 %s 上无鉴权监听：HTTP 端点可以驱动 agent 执行工具；"+
			"请配置 WithHTTPAuthToken，或在已受保护的网络中显式使用 WithHTTPInsecure()", addr)
	}
	return net.JoinHostPort(host, port), true, nil
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}
