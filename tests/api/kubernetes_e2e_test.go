package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/integration/kubernetes"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// k3sImage: cluster Kubernetes một node dùng cho test đầu-cuối. Đổi bằng HQ_TEST_K3S_IMAGE.
const k3sImage = "rancher/k3s:v1.31.2-k3s1"

// TestKubernetesEndToEnd chạy integration Kubernetes với một kube-apiserver THẬT (k3s
// trong container, cần Docker chạy được container privileged), qua Gateway. Chỉ chạy khi
// HQ_TEST_K3S=1: container privileged và việc kéo image của cluster không phù hợp với
// mọi máy CI.
func TestKubernetesEndToEnd(t *testing.T) {
	if testing.Short() || os.Getenv("HQ_TEST_K3S") != "1" {
		t.Skip("đặt HQ_TEST_K3S=1 để chạy (cần Docker cho phép container privileged)")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	image := k3sImage
	if v := os.Getenv("HQ_TEST_K3S_IMAGE"); v != "" {
		image = v
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Cmd:          []string{"server", "--disable=traefik", "--disable=servicelb", "--disable=metrics-server"},
			ExposedPorts: []string{"6443/tcp"},
			Privileged:   true,
			Tmpfs:        map[string]string{"/run": "", "/var/run": ""},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start k3s: %v", err)
	}
	sh := func(cmd string) (string, bool) {
		code, out, err := ctr.Exec(ctx, []string{"sh", "-c", cmd}, tcexec.Multiplexed())
		if err != nil {
			return err.Error(), false
		}
		b, _ := io.ReadAll(io.LimitReader(out, 1<<20))
		return strings.TrimSpace(string(b)), code == 0
	}
	must := func(cmd string) string {
		t.Helper()
		out, ok := sh(cmd)
		if !ok {
			t.Fatalf("%s: %s", cmd, out)
		}
		return out
	}
	// Chờ cluster sẵn sàng và CoreDNS chạy (có trần): đó là workload thật để đọc.
	ready := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline) && !ready; time.Sleep(3 * time.Second) {
		out, ok := sh(`kubectl -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{.items[*].status.phase}'`)
		ready = ok && strings.Contains(out, "Running")
	}
	if !ready {
		t.Fatal("k3s không sẵn sàng (CoreDNS chưa Running) trong 5 phút")
	}
	// Service account chỉ đọc, như runbook: clusterrole view + đọc node.
	must(`kubectl -n default create serviceaccount hq-reader`)
	must(`kubectl create clusterrolebinding hq-reader-view --clusterrole=view --serviceaccount=default:hq-reader`)
	must(`kubectl create clusterrole hq-nodes --verb=get,list --resource=nodes`)
	must(`kubectl create clusterrolebinding hq-reader-nodes --clusterrole=hq-nodes --serviceaccount=default:hq-reader`)
	// Một Secret và một pod mang env nhạy cảm: không tool nào được trả chúng.
	must(`kubectl -n default create secret generic db --from-literal=password=MAT-KHAU-BI-MAT`)
	must(`kubectl -n default run leaky --image=rancher/mirrored-pause:3.6 --env=API_TOKEN=TOKEN-BI-MAT --labels=app=leaky --annotations=note=CHU-THICH-BI-MAT`)
	token := must(`kubectl -n default create token hq-reader --duration=1h`)
	ca := must(`cat /var/lib/rancher/k3s/server/tls/server-ca.crt`)
	endpoint, err := ctr.PortEndpoint(ctx, "6443/tcp", "https")
	if err != nil {
		t.Fatal(err)
	}

	signer, _ := signedctx.NewSigner(signingKey)
	connect := func(slugSuffix string, namespaces []string) func(string, map[string]any) (string, bool) {
		t.Helper()
		client, err := kubernetes.NewClient(endpoint, kubernetes.Options{BearerToken: token, CAPEM: []byte(ca), Namespaces: namespaces})
		if err != nil {
			t.Fatal(err)
		}
		up := httptest.NewServer(kubernetes.Handler(client, signer, e.orgA))
		t.Cleanup(up.Close)
		if slugSuffix == "" {
			conn, err := e.db.Admin.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: kubernetes.Slug}, "Kubernetes", up.URL)
			conn.Release()
			if err != nil {
				t.Fatalf("đăng ký server: %v", err)
			}
		}
		projA := e.createProject(e.tok("admin-a"), "k8s-a"+slugSuffix, "K8s A")["id"].(string)
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: e.gw.URL + "/mcp/v1/projects/" + projA, HTTPClient: &http.Client{Transport: bearer{e.tok("admin-a")}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return func(name string, args map[string]any) (string, bool) {
			t.Helper()
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "kubernetes." + name, Arguments: args})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var b strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					b.WriteString(tc.Text)
				}
			}
			return b.String(), res.IsError
		}
	}
	call := connect("", nil)
	var all strings.Builder
	check := func(name string, args map[string]any, wantSubstr ...string) string {
		t.Helper()
		out, isErr := call(name, args)
		all.WriteString(out)
		if isErr {
			t.Errorf("%s: lỗi tool: %s", name, out)
			return out
		}
		for _, w := range wantSubstr {
			if !strings.Contains(out, w) {
				t.Errorf("%s: thiếu %q trong %s", name, w, out)
			}
		}
		return out
	}
	check("list_namespaces", map[string]any{}, `"kube-system"`, `"default"`)
	check("list_nodes", map[string]any{}, `"ready":"True"`, "kubelet_version")
	pods := check("list_pods", map[string]any{"namespace": "kube-system", "label_selector": "k8s-app=kube-dns"}, "coredns", "Running")
	// Tên pod CoreDNS lấy từ chính kết quả của tool.
	podName := ""
	if i := strings.Index(pods, `"coredns-`); i >= 0 {
		podName = pods[i+1 : i+1+strings.Index(pods[i+1:], `"`)]
	}
	if podName == "" {
		t.Fatalf("không tìm thấy tên pod CoreDNS trong %s", pods)
	}
	check("get_pod", map[string]any{"namespace": "kube-system", "name": podName}, "coredns", `"ready":true`)
	// Phân trang qua Gateway: con trỏ không bị lớp che secret của Gateway làm hỏng, và
	// trang hai đọc được bằng chính con trỏ đó.
	first := check("list_pods", map[string]any{"namespace": "kube-system", "limit": 1}, `"next_cursor":"`)
	if strings.Contains(first, "REDACTED") {
		t.Fatalf("con trỏ bị Gateway che: %s", first)
	}
	cur := first[strings.Index(first, `"next_cursor":"`)+len(`"next_cursor":"`):]
	cur = cur[:strings.Index(cur, `"`)]
	if second := check("list_pods", map[string]any{"namespace": "kube-system", "limit": 1, "cursor": cur}, `"pods":[{`); second == first {
		t.Errorf("trang hai trùng trang một: %s", second)
	}
	if out := check("get_pod_logs", map[string]any{"namespace": "kube-system", "pod": podName, "tail_lines": 20}, `"logs":"`); !strings.Contains(strings.ToLower(out), "coredns") {
		t.Errorf("get_pod_logs không có log của CoreDNS: %s", out)
	}
	check("list_deployments", map[string]any{"namespace": "kube-system"}, `"coredns"`)
	check("get_deployment", map[string]any{"namespace": "kube-system", "name": "coredns"}, `"coredns"`, "image")
	check("list_events", map[string]any{"namespace": "default", "involved_kind": "Pod", "involved_name": "leaky"}, "leaky")
	check("list_pods", map[string]any{"namespace": "default"}, "leaky")
	check("get_pod", map[string]any{"namespace": "default", "name": "leaky"}, "leaky")
	// Không gì nhạy cảm lọt ra từ một cluster thật: env, annotation, Secret, và token của server.
	for _, secret := range []string{"TOKEN-BI-MAT", "MAT-KHAU-BI-MAT", "CHU-THICH-BI-MAT", "API_TOKEN", token} {
		if strings.Contains(all.String(), secret) {
			t.Errorf("kết quả tool chứa %q", secret)
		}
	}
	// Tên không hợp lệ và tài nguyên không tồn tại là lỗi tool, không lộ địa chỉ apiserver.
	if out, isErr := call("get_pod", map[string]any{"namespace": "default", "name": "khong-co"}); !isErr || strings.Contains(out, endpoint) {
		t.Errorf("pod không tồn tại: err=%v %s", isErr, out)
	}
	if out, isErr := call("get_pod", map[string]any{"namespace": "default", "name": "../secrets/db"}); !isErr {
		t.Errorf("tên thoát đường dẫn được chấp nhận: %s", out)
	}
}
