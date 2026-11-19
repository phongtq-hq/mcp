// Package safehttp là HTTP client có bảo vệ SSRF cho mọi URL do người dùng cung cấp
// (endpoint MCP của server ngoài, webhook). Xem security §6.
//
// Quy tắc, áp dụng ở lúc đăng ký VÀ ở mỗi lần gọi:
//   - chỉ https (trừ khi policy cho phép http ở môi trường cục bộ), cổng trong danh sách;
//   - phân giải DNS rồi kiểm tra MỌI địa chỉ trả về: loopback, private, link-local
//     (gồm metadata 169.254.169.254), CGNAT, ULA, các dải dành riêng và mọi dạng
//     IPv4 nhúng trong IPv6 đều bị từ chối;
//   - kết nối tới đúng IP đã kiểm tra (ghim ở dialer), chống DNS rebinding;
//   - không tự theo redirect;
//   - giới hạn thời gian và kích thước phản hồi.
//
// Không tạo http.Client trần cho URL ngoài; dùng Policy.Client.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrBlocked báo URL hoặc địa chỉ đích bị chính sách từ chối. Thông báo không chứa
// địa chỉ đã phân giải: nó không được trở thành công cụ dò mạng nội bộ.
var ErrBlocked = errors.New("safehttp: địa chỉ đích không được phép")

// Policy là chính sách cho URL ngoài.
type Policy struct {
	// Ports là các cổng được phép. Rỗng = 443 và 8443.
	Ports []int
	// AllowInsecureLocal cho phép http và địa chỉ loopback / private. CHỈ dành cho
	// phát triển cục bộ và test: bật nó là tắt bảo vệ SSRF.
	AllowInsecureLocal bool
	// Resolver dùng để phân giải tên; nil = resolver mặc định.
	Resolver *net.Resolver
}

var defaultPorts = []int{443, 8443}

// blocked là các dải không bao giờ là đích hợp lệ của một URL do người dùng cung cấp.
var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/96", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// AllowedAddr cho biết một địa chỉ có phải đích công khai hợp lệ hay không.
func AllowedAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	// IPv4 ánh xạ trong IPv6 (::ffff:a.b.c.d) được xét như IPv4.
	a = a.Unmap().WithZone("")
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	// IPv6: chỉ địa chỉ unicast toàn cầu (2000::/3) là đích hợp lệ. Mọi dải khác, kể cả
	// dải được cấp về sau cho mục đích đặc biệt, bị từ chối theo mặc định.
	if a.Is6() && !globalUnicast6.Contains(a) {
		return false
	}
	// Phòng khi thư viện chuẩn biết thêm dải nào đó.
	return !a.IsLoopback() && !a.IsPrivate() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() &&
		!a.IsMulticast() && !a.IsUnspecified() && !a.IsInterfaceLocalMulticast()
}

func (p Policy) ports() []int {
	if len(p.Ports) == 0 {
		return defaultPorts
	}
	return p.Ports
}

// CheckURL kiểm tra dạng của URL (không phân giải DNS): scheme, cổng, không kèm thông
// tin đăng nhập, và nếu host là IP thì IP đó phải được phép. Trả về URL đã chuẩn hoá.
func (p Policy) CheckURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, fmt.Errorf("%w: URL quá dài", ErrBlocked)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: URL không hợp lệ", ErrBlocked)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: URL không được kèm thông tin đăng nhập", ErrBlocked)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("%w: URL không được có fragment", ErrBlocked)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecureLocal {
			return nil, fmt.Errorf("%w: chỉ chấp nhận https", ErrBlocked)
		}
	default:
		return nil, fmt.Errorf("%w: chỉ chấp nhận https", ErrBlocked)
	}
	if !p.AllowInsecureLocal {
		port := 443
		if s := u.Port(); s != "" {
			if port, err = strconv.Atoi(s); err != nil {
				return nil, fmt.Errorf("%w: cổng không hợp lệ", ErrBlocked)
			}
		}
		if !slices.Contains(p.ports(), port) {
			return nil, fmt.Errorf("%w: cổng không nằm trong danh sách cho phép", ErrBlocked)
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
			return nil, ErrBlocked
		}
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !AllowedAddr(ip) {
			return nil, ErrBlocked
		}
	}
	return u, nil
}

// Resolve phân giải host và trả về các địa chỉ, chỉ khi MỌI địa chỉ đều được phép:
// một tên trả về cả địa chỉ công khai lẫn địa chỉ nội bộ bị từ chối toàn bộ.
func (p Policy) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !p.AllowInsecureLocal && !AllowedAddr(ip) {
			return nil, ErrBlocked
		}
		return []netip.Addr{ip}, nil
	}
	r := p.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("%w: không phân giải được tên", ErrBlocked)
	}
	if !p.AllowInsecureLocal {
		for _, a := range addrs {
			if !AllowedAddr(a) {
				return nil, ErrBlocked
			}
		}
	}
	return addrs, nil
}

// Check kiểm tra đầy đủ một URL: dạng của nó và mọi địa chỉ nó phân giải tới. Dùng
// lúc đăng ký; mỗi lần gọi được kiểm lại ở dialer của Client.
func (p Policy) Check(ctx context.Context, raw string) (*url.URL, error) {
	u, err := p.CheckURL(raw)
	if err != nil {
		return nil, err
	}
	if _, err := p.Resolve(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return u, nil
}

// Transport trả về http.RoundTripper có bảo vệ: mỗi kết nối phân giải lại tên, kiểm
// mọi địa chỉ, rồi nối tới đúng địa chỉ đã kiểm (tên không được phân giải lần hai).
func (p Policy) Transport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil, // không qua proxy của môi trường: proxy sẽ là bên phân giải tên
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, ErrBlocked
			}
			addrs, err := p.Resolve(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, a := range addrs {
				conn, err := dialer.DialContext(ctx, network, netip.AddrPortFrom(a, mustPort(port)).String())
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			_ = lastErr // chi tiết kết nối không lộ ra ngoài
			return nil, errors.New("safehttp: không kết nối được tới đích")
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func mustPort(s string) uint16 {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// guard kiểm URL của từng request (kể cả request do thư viện phía trên tạo ra) và
// giới hạn kích thước phản hồi.
type guard struct {
	policy  Policy
	base    http.RoundTripper
	maxBody int64
}

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, err := g.policy.CheckURL(r.URL.String()); err != nil {
		return nil, err
	}
	resp, err := g.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if g.maxBody > 0 {
		resp.Body = &limited{ReadCloser: resp.Body, left: g.maxBody}
	}
	return resp, nil
}

type limited struct {
	io.ReadCloser
	left int64
}

// ErrTooLarge báo phản hồi vượt giới hạn kích thước.
var ErrTooLarge = errors.New("safehttp: phản hồi vượt giới hạn kích thước")

func (l *limited) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.ReadCloser.Read(p)
	l.left -= int64(n)
	return n, err
}

// RoundTripper bọc một transport đã có bảo vệ bằng lớp kiểm URL và giới hạn phản hồi.
func (p Policy) RoundTripper(transport http.RoundTripper, maxResponseBytes int64) http.RoundTripper {
	return guard{policy: p, base: transport, maxBody: maxResponseBytes}
}

// Client trả về http.Client cho URL ngoài: có bảo vệ SSRF, không theo redirect, có
// giới hạn thời gian và kích thước phản hồi.
func (p Policy) Client(timeout time.Duration, maxResponseBytes int64) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: p.RoundTripper(p.Transport(), maxResponseBytes),
		// Redirect không được theo tự động: đích mới phải được kiểm như một URL mới.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
