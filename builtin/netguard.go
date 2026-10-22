package builtin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// 网络访问防护：阻止 WebFetch 被用来访问本机、内网与云元数据地址（SSRF）。
// 模型读到的网页内容可能含有注入指令，诱导它去抓取 http://169.254.169.254/
// 或内网服务并把结果带回对话——WebFetch 是只读级工具，不经用户审批。

// blockedPrefixes 是 netip 的分类方法覆盖不到、但同样不该从公网工具访问的网段。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "本网络"
	netip.MustParsePrefix("100.64.0.0/10"),  // 运营商级 NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF 协议分配
	netip.MustParsePrefix("198.18.0.0/15"),  // 基准测试
	netip.MustParsePrefix("240.0.0.0/4"),    // 保留（含广播地址）
	netip.MustParsePrefix("64:ff9b:1::/48"), // 本地 NAT64
	netip.MustParsePrefix("fec0::/10"),      // 已废弃的站点本地地址
	netip.MustParsePrefix("2001:db8::/32"),  // 文档示例
	netip.MustParsePrefix("100::/64"),       // 丢弃前缀
	netip.MustParsePrefix("2001::/23"),      // IETF 协议分配（含 Teredo）
}

var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// isPublicAddr 报告 addr 是否是可以从公网工具访问的地址。
func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	// 内嵌 IPv4 的 IPv6 地址（NAT64 64:ff9b::/96、6to4 2002::/16）按内嵌的
	// IPv4 判断，否则可借此绕过上面的检查。
	if addr.Is6() {
		b := addr.As16()
		if nat64Prefix.Contains(addr) {
			return isPublicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		}
		if sixToFour.Contains(addr) {
			return isPublicAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return true
}

// errBlockedAddr 是访问被拦截时返回给模型的错误。
type errBlockedAddr struct{ addr string }

func (e errBlockedAddr) Error() string {
	return fmt.Sprintf("拒绝访问 %s：WebFetch 只能访问公网地址（本机、内网、链路本地与云元数据地址被禁止）", e.addr)
}

// guardDial 在建立 TCP 连接的那一刻检查目标 IP：域名解析之后、连接之前，
// 覆盖重定向与 DNS 重绑定。
func guardDial(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errBlockedAddr{addr: address}
	}
	if !isPublicAddr(ap.Addr()) {
		return errBlockedAddr{addr: ap.Addr().String()}
	}
	return nil
}

// publicOnlyClient 返回只能连接公网地址的 HTTP 客户端。
//
// 走代理（HTTP_PROXY/HTTPS_PROXY）时，连接对象是代理本身，目标地址由代理
// 解析；此时改为在发请求前解析目标主机并检查（防不住 DNS 重绑定，代理侧的
// 访问控制由运维负责）。
func publicOnlyClient(timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	guarded := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: guardDial}
	plain := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if viaProxy, _ := ctx.Value(proxyDialKey{}).(bool); viaProxy {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &proxyAwareTransport{base: base},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			return checkURLScheme(req.URL)
		},
	}
}

type proxyDialKey struct{}

// proxyAwareTransport 在请求会经过代理时标记 ctx（放行到代理的连接），并在
// 发出前检查目标主机解析出的地址。
type proxyAwareTransport struct {
	base *http.Transport
}

func (t *proxyAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkURLScheme(req.URL); err != nil {
		return nil, err
	}
	proxyURL, err := t.base.Proxy(req)
	if err != nil {
		return nil, err
	}
	if proxyURL != nil {
		if err := checkHostResolvesPublic(req.Context(), req.URL.Hostname()); err != nil {
			return nil, err
		}
		req = req.WithContext(context.WithValue(req.Context(), proxyDialKey{}, true))
	}
	return t.base.RoundTrip(req)
}

func checkURLScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("不支持的 URL 协议: %s", u.Scheme)
	}
	return nil
}

// checkHostResolvesPublic 解析主机名，任一地址不是公网地址即拒绝。
func checkHostResolvesPublic(ctx context.Context, host string) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddr(addr) {
			return errBlockedAddr{addr: host}
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("解析主机 %s 失败: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicAddr(ip) {
			return errBlockedAddr{addr: host + "（" + ip.String() + "）"}
		}
	}
	return nil
}
