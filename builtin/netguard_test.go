package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Dream355873200/GoAgent"
)

func TestIsPublicAddr(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"1.1.1.1":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false, // 云元数据
		"100.64.0.1":           false, // 运营商级 NAT
		"0.0.0.0":              false,
		"255.255.255.255":      false,
		"224.0.0.1":            false,
		"::1":                  false,
		"::":                   false,
		"fe80::1":              false,
		"fc00::1":              false,
		"::ffff:127.0.0.1":     false, // IPv4 映射
		"::ffff:10.0.0.1":      false,
		"64:ff9b::7f00:1":      false, // NAT64 内嵌 127.0.0.1
		"64:ff9b::808:808":     true,  // NAT64 内嵌 8.8.8.8
		"2002:7f00:1::":        false, // 6to4 内嵌 127.0.0.1
		"2002:a00:1::":         false, // 6to4 内嵌 10.0.0.1
	}
	for s, want := range cases {
		if got := isPublicAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestWebFetchBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret")
	}))
	defer srv.Close()

	call := func(def goagent.ToolDef, u string) (string, error) {
		fn := def.Execute.(func(goagent.Context, WebFetchInput) (string, error))
		return fn(goagent.Context{Context: context.Background()}, WebFetchInput{URL: u})
	}

	out, err := call(WebFetchTool(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "拒绝访问") {
		t.Fatalf("默认应拒绝访问本机地址: out=%q err=%v", out, err)
	}
	// 用主机名指向回环地址同样被拦截（连接时按解析结果检查）。
	local := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	if _, err := call(WebFetchTool(), local); err == nil {
		t.Fatal("localhost 应被拒绝")
	}

	out, err = call(WebFetchToolWith(WebFetchConfig{AllowPrivateNetwork: true}), srv.URL)
	if err != nil || out != "secret" {
		t.Fatalf("显式允许内网时应能访问: out=%q err=%v", out, err)
	}

	if _, err := call(WebFetchTool(), "file:///etc/passwd"); err == nil {
		t.Fatal("非 http(s) 协议应被拒绝")
	}
}

func TestCheckHostResolvesPublic(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "169.254.169.254", "::1"} {
		if err := checkHostResolvesPublic(context.Background(), h); err == nil {
			t.Errorf("%s 应被拒绝", h)
		}
	}
	if err := checkHostResolvesPublic(context.Background(), "93.184.216.34"); err != nil {
		t.Errorf("公网 IP 应放行: %v", err)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("中", 10) // 每个 3 字节
	got := truncateUTF8(s, 7)
	if got != "中中..." {
		t.Fatalf("truncateUTF8 = %q", got)
	}
}
