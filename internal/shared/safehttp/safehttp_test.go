package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllowedAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "127.255.255.254": false, "10.0.0.5": false, "172.16.0.1": false, "172.31.255.255": false,
		"192.168.1.1": false, "169.254.169.254": false, "100.64.0.1": false, "100.127.255.255": false, "0.0.0.0": false,
		"192.0.0.8": false, "198.18.0.1": false, "224.0.0.1": false, "240.0.0.1": false, "255.255.255.255": false,
		"::1": false, "::": false, "fc00::1": false, "fd12:3456::1": false, "fe80::1": false, "ff02::1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "::ffff:169.254.169.254": false, "::ffff:8.8.8.8": true,
		"::127.0.0.1": false, "::a9fe:a9fe": false, "::ffff:0:127.0.0.1": false, "fec0::1": false, "3fff::1": false, "5f00::1": false,
		"4000::1": false, "e000::1": false, "2a00:1450:4001::1": true,
		"64:ff9b::7f00:1": false, "2002:7f00:1::": false, "2001:db8::1": false, "fe80::1%eth0": false,
	} {
		if got := AllowedAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: allowed = %v, muốn %v", addr, got, want)
		}
	}
	if AllowedAddr(netip.Addr{}) {
		t.Error("địa chỉ rỗng được phép")
	}
}

func TestCheckURL(t *testing.T) {
	p := Policy{}
	for raw, ok := range map[string]bool{
		"https://mcp.example.com/mcp": true, "https://mcp.example.com:8443/mcp": true, "https://8.8.8.8/mcp": true,
		"http://mcp.example.com/mcp": false, "ftp://example.com": false, "file:///etc/passwd": false, "gopher://x": false,
		"https://mcp.example.com:22/": false, "https://mcp.example.com:6379/": false,
		"https://user:pass@mcp.example.com/": false, "https://mcp.example.com/#frag": false,
		"https://[::127.0.0.1]/": false, "https://[fec0::1]/": false,
		"https://127.0.0.1/": false, "https://[::1]/": false, "https://169.254.169.254/latest/meta-data/": false,
		"https://[::ffff:10.0.0.1]/": false, "https://10.0.0.1:8443/": false, "https://localhost/": false,
		"https://foo.localhost/": false, "https://db.internal/": false, "https://printer.local/": false, "https://LOCALHOST./": false,
		"https://": false, "": false, "//example.com": false, "https://example.com:notaport/": false,
		"https://" + strings.Repeat("a", 3000) + ".com/": false,
	} {
		if _, err := p.CheckURL(raw); (err == nil) != ok {
			t.Errorf("%q: err = %v, muốn hợp lệ = %v", raw, err, ok)
		}
	}
	// Ngoại lệ cho phát triển cục bộ phải được bật tường minh.
	if _, err := (Policy{AllowInsecureLocal: true}).CheckURL("http://127.0.0.1:8090/mcp"); err != nil {
		t.Errorf("AllowInsecureLocal: %v", err)
	}
}

// answerSet là tập địa chỉ mà DNS giả trả về, đổi được an toàn giữa chừng.
type answerSet struct {
	mu  sync.Mutex
	ips []string
}

func (a *answerSet) set(ips ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ips = ips
}

func (a *answerSet) get() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ips...)
}

// resolverTo trả về resolver luôn phân giải mọi tên thành các địa chỉ đã cho.
func resolverTo(t *testing.T, answers func() []string) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(dnsAnswer(buf[:n], answers()), addr)
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

// dnsAnswer dựng phản hồi DNS tối thiểu: bản ghi A cho câu hỏi A, rỗng cho loại khác.
func dnsAnswer(q []byte, ips []string) []byte {
	if len(q) < 12 {
		return nil
	}
	end := 12
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	end += 5 // byte 0 + qtype + qclass
	if end > len(q) {
		return nil
	}
	qtype := int(q[end-4])<<8 | int(q[end-3])
	out := append([]byte{}, q[:end]...)
	out[2], out[3] = 0x81, 0x80
	count := 0
	if qtype == 1 {
		for _, ip := range ips {
			a := netip.MustParseAddr(ip).As4()
			out = append(out, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4)
			out = append(out, a[:]...)
			count++
		}
	}
	out[6], out[7] = 0, byte(count)
	out[8], out[9], out[10], out[11] = 0, 0, 0, 0
	return out
}

func TestResolveRejectsAnyInternalAddress(t *testing.T) {
	answers := &answerSet{ips: []string{"8.8.8.8"}}
	p := Policy{Resolver: resolverTo(t, answers.get)}
	ctx := context.Background()
	if _, err := p.Check(ctx, "https://mcp.example.com/mcp"); err != nil {
		t.Fatalf("tên công khai: %v", err)
	}
	for name, ips := range map[string][]string{
		"chỉ nội bộ":              {"10.0.0.5"},
		"metadata":                {"169.254.169.254"},
		"loopback":                {"127.0.0.1"},
		"lẫn công khai và nội bộ": {"8.8.8.8", "192.168.1.10"},
	} {
		answers.set(ips...)
		if _, err := p.Check(ctx, "https://mcp.example.com/mcp"); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: err = %v, muốn ErrBlocked", name, err)
		}
	}
}

// DNS rebinding: tên trả về địa chỉ công khai lúc kiểm tra, rồi địa chỉ nội bộ lúc
// kết nối. Dialer phân giải và kiểm lại, nên kết nối không tới được dịch vụ nội bộ.
func TestClientBlocksRebindingAndInternalTargets(t *testing.T) {
	var hit atomic.Bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		_, _ = io.WriteString(w, "SECRET INTERNAL DATA")
	}))
	defer internal.Close()
	port := internal.URL[strings.LastIndex(internal.URL, ":")+1:]

	answers := &answerSet{ips: []string{"8.8.8.8"}}
	p := Policy{Resolver: resolverTo(t, answers.get), Ports: []int{443, mustInt(port)}}
	if _, err := p.Check(context.Background(), "https://rebind.example.com:"+port+"/"); err != nil {
		t.Fatalf("kiểm tra lúc đăng ký: %v", err)
	}
	answers.set("127.0.0.1") // tên đổi đích sau khi đã được duyệt
	client := p.Client(5*time.Second, 1<<20)
	client.Transport.(guard).base.(*http.Transport).TLSClientConfig = nil
	for _, target := range []string{"https://rebind.example.com:" + port + "/", "https://127.0.0.1:" + port + "/", "http://127.0.0.1:" + port + "/"} {
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: request đi được", target)
		}
	}
	if hit.Load() {
		t.Error("request tới được dịch vụ nội bộ")
	}
}

func TestClientLimitsAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("x", 4096))
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	defer srv.Close()
	client := Policy{AllowInsecureLocal: true}.Client(5*time.Second, 1024)

	resp, err := client.Get(srv.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("redirect bị theo tự động: status %d", resp.StatusCode)
	}
	resp, err = client.Get(srv.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, ErrTooLarge) || len(body) > 1024 {
		t.Errorf("phản hồi lớn: đọc %d byte, err = %v", len(body), err)
	}
}

func mustInt(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
