package kubernetes_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/kubernetes"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// recorder ghi lại mọi request mà apiserver giả lập nhận được.
type recorder struct {
	mu   sync.Mutex
	seen []string
	auth []string
	last map[string]string // path → query string của request gần nhất
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.last[req.URL.Path] = req.URL.RawQuery
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *recorder) lastQuery(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[path]
}

// Những chuỗi nằm trong dữ liệu của apiserver giả lập mà không tool nào được trả ra.
var secrets = []string{"env-secret", "envfrom-secret", "arg-secret", "cmd-secret", "annotation-secret", "label-secret",
	"managed-secret", "db-credentials", "/host-secret", "pull-secret", "pw-secret", "q-secret", "mount-secret",
	"probe-secret", "taint-value-secret", "node-label-secret", "image-list-secret", "bearer-in-message",
	"hunter2secret", "term-secret", "status-secret", "couldn't find key", "secret-event-leak", "configmap-event-leak"}

// podJSON là một pod theo định dạng của core/v1, đầy những trường nhạy cảm mà server
// không được đọc.
func podJSON(ns, name, node string) string {
	return fmt.Sprintf(`{"kind":"Pod","apiVersion":"v1",
 "metadata":{"name":%q,"namespace":%q,"creationTimestamp":"2026-11-18T08:00:00Z","generation":1,
  "labels":{"app":"web","team":"label-secret"},
  "annotations":{"vault.hashicorp.com/token":"annotation-secret","kubectl.kubernetes.io/last-applied-configuration":"{\"env\":\"annotation-secret\"}"},
  "managedFields":[{"manager":"kubectl","fieldsV1":{"f:managed-secret":{}}}],
  "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"web-7d9f","uid":"u1"}]},
 "spec":{"nodeName":%q,"serviceAccountName":"web","restartPolicy":"Always",
  "imagePullSecrets":[{"name":"pull-secret"}],
  "initContainers":[{"name":"migrate","image":"registry.example/migrate:3","command":["sh","-c","cmd-secret"],
    "env":[{"name":"DSN","value":"postgres://app:env-secret@db/app"}]}],
  "containers":[{"name":"app","image":"registry.example/web:1.4.2",
    "command":["/bin/web","--token=cmd-secret"],"args":["--password=arg-secret"],
    "env":[{"name":"API_KEY","value":"env-secret"},{"name":"FROM","valueFrom":{"secretKeyRef":{"name":"db-credentials","key":"password"}}}],
    "envFrom":[{"secretRef":{"name":"envfrom-secret"}}],
    "ports":[{"name":"http","containerPort":8080,"protocol":"TCP"},{"containerPort":9090}],
    "resources":{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":"1","memory":"512Mi","example.com/gpu":"1"}},
    "volumeMounts":[{"name":"creds","mountPath":"/mount-secret"}],
    "livenessProbe":{"httpGet":{"path":"/healthz?key=probe-secret","port":8080}}},
   {"name":"sidecar","image":"registry.example/proxy:2"},
   {"name":"cfg","image":"registry.example/cfg:1"},{"name":"runner","image":"registry.example/runner:1"}],
  "volumes":[{"name":"creds","secret":{"secretName":"db-credentials"}},
   {"name":"host","hostPath":{"path":"/host-secret/etc"}},
   {"name":"cache","emptyDir":{}},
   {"name":"strange","flexVolume":{"driver":"x","options":{"password":"env-secret"}}}]},
 "status":{"phase":"Running","reason":"NodeAffinity","message":"secret \"status-secret\" not found while admitting pod","podIP":"10.1.2.3","hostIP":"192.168.0.10","startTime":"2026-11-18T08:00:05Z","qosClass":"Burstable",
  "conditions":[{"type":"Ready","status":"False","reason":"ContainersNotReady","message":"containers with unready status: [app]","lastTransitionTime":"2026-11-18T08:10:00Z"}],
  "initContainerStatuses":[{"name":"migrate","ready":true,"restartCount":0,"state":{"terminated":{"exitCode":0,"reason":"Completed","message":"migrated with DSN postgres://app:term-secret@db/app","finishedAt":"2026-11-18T08:00:04Z"}}}],
  "containerStatuses":[
   {"name":"app","ready":false,"restartCount":7,"image":"registry.example/web:1.4.2",
    "state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s restarting failed container; last fetch https://puller:pw-secret@registry.example/v2/?token=q-secret"}},
    "lastState":{"terminated":{"exitCode":137,"signal":9,"reason":"OOMKilled","message":"panic: term-secret","finishedAt":"2026-11-18T08:09:00Z"}}},
   {"name":"cfg","ready":false,"restartCount":0,"state":{"waiting":{"reason":"CreateContainerConfigError","message":"couldn't find key password in Secret default/db-credentials"}}},
   {"name":"runner","ready":false,"restartCount":0,"state":{"waiting":{"reason":"RunContainerError","message":"exec: \"/bin/web --db-pass=hunter2secret\": no such file"}}},
   {"name":"sidecar","ready":true,"restartCount":1,"state":{"running":{"startedAt":"2026-11-18T08:00:06Z"}}}]}}`, name, ns, node)
}

func deploymentJSON(ns, name string) string {
	return fmt.Sprintf(`{"kind":"Deployment","apiVersion":"apps/v1",
 "metadata":{"name":%q,"namespace":%q,"generation":4,"creationTimestamp":"2026-10-01T00:00:00Z",
  "labels":{"team":"label-secret"},"annotations":{"deployment.kubernetes.io/revision":"4","note":"annotation-secret"},
  "managedFields":[{"manager":"managed-secret"}]},
 "spec":{"replicas":3,"strategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"team":"label-secret"}},
  "template":{"metadata":{"annotations":{"x":"annotation-secret"}},"spec":{"containers":[
   {"name":"app","image":"registry.example/web:1.4.2","env":[{"name":"API_KEY","value":"env-secret"}],"args":["--password=arg-secret"]}],
   "volumes":[{"name":"creds","secret":{"secretName":"db-credentials"}}]}}},
 "status":{"observedGeneration":3,"replicas":3,"updatedReplicas":1,"readyReplicas":2,"availableReplicas":2,"unavailableReplicas":1,
  "conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"ReplicaSet \"web-7d9f\" has timed out progressing.","lastTransitionTime":"2026-11-18T08:20:00Z"}]}}`, name, ns)
}

const nodesItems = `{"metadata":{"name":"node-a","creationTimestamp":"2026-01-01T00:00:00Z",
  "labels":{"node-role.kubernetes.io/control-plane":"","topology.kubernetes.io/zone":"zone-1","node.kubernetes.io/instance-type":"m5.large","team":"node-label-secret"},
  "annotations":{"csi.volume.kubernetes.io/nodeid":"annotation-secret"}},
 "spec":{"unschedulable":true,"taints":[{"key":"dedicated","value":"taint-value-secret","effect":"NoSchedule"}]},
 "status":{"capacity":{"cpu":"4","memory":"16Gi","pods":"110"},"allocatable":{"cpu":"3900m","memory":"15Gi","pods":"110"},
  "conditions":[{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"True","reason":"KubeletHasDiskPressure"},
   {"type":"Ready","status":"True","reason":"KubeletReady","lastTransitionTime":"2026-11-01T00:00:00Z"}],
  "addresses":[{"type":"InternalIP","address":"192.168.0.10"},{"type":"Hostname","address":"node-a"}],
  "nodeInfo":{"kubeletVersion":"v1.34.1","osImage":"Ubuntu 24.04","kernelVersion":"6.8.0","architecture":"amd64","containerRuntimeVersion":"containerd://2.1.0"},
  "images":[{"names":["registry.example/image-list-secret:1"],"sizeBytes":1}]}},
 {"metadata":{"name":"node-b","creationTimestamp":"2026-01-02T00:00:00Z","labels":{"node-role.kubernetes.io/worker":""}},
  "spec":{},"status":{"conditions":[{"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown"}],
  "capacity":{"cpu":"8","memory":"32Gi","pods":"110"},"allocatable":{"cpu":"8","memory":"31Gi","pods":"110"},"nodeInfo":{"kubeletVersion":"v1.34.1"}}}`

const eventsItems = `{"metadata":{"name":"api-0.1","namespace":"default","creationTimestamp":"2026-11-18T08:00:00Z","managedFields":[{"manager":"managed-secret"}]},
  "involvedObject":{"kind":"Pod","namespace":"default","name":"api-0","uid":"u1"},"type":"Normal","reason":"Scheduled",
  "message":"Successfully assigned default/api-0 to node-a","count":1,"firstTimestamp":"2026-11-18T08:00:00Z","lastTimestamp":"2026-11-18T08:00:00Z","source":{"component":"default-scheduler"}},
 {"metadata":{"name":"api-0.2","namespace":"default","creationTimestamp":"2026-11-18T08:05:00Z"},
  "involvedObject":{"kind":"Pod","namespace":"default","name":"api-0"},"type":"Warning","reason":"Failed",
  "message":"Failed to pull image: Get \"https://puller:pw-secret@registry.example/v2/?token=q-secret\": denied, Authorization: Bearer bearer-in-message",
  "count":12,"firstTimestamp":"2026-11-18T08:01:00Z","lastTimestamp":"2026-11-18T08:30:00Z","source":{"component":"kubelet"}},
 {"metadata":{"name":"web.3","namespace":"default","creationTimestamp":"2026-11-18T08:10:00Z"},
  "involvedObject":{"kind":"Deployment","namespace":"default","name":"web"},"type":"Normal","reason":"ScalingReplicaSet",
  "message":"Scaled up replica set web-7d9f to 3","eventTime":"2026-11-18T08:10:00.000000Z","reportingComponent":"deployment-controller",
  "series":{"count":4,"lastObservedTime":"2026-11-18T08:20:00.000000Z"}},
 {"metadata":{"name":"s.4","namespace":"default"},"involvedObject":{"kind":"Secret","namespace":"default","name":"db-credentials"},"type":"Warning","reason":"SyncFailed",
  "message":"secret-event-leak: could not sync key","count":1,"lastTimestamp":"2026-11-18T09:00:00Z"},
 {"metadata":{"name":"c.5","namespace":"default"},"involvedObject":{"kind":"ConfigMap","namespace":"default","name":"app-config"},"type":"Normal","reason":"Updated",
  "message":"configmap-event-leak","count":1,"lastTimestamp":"2026-11-18T09:00:00Z"},
 {"metadata":{"name":"n.6","namespace":"default"},"involvedObject":{"kind":"Node","name":"node-a"},"type":"Normal","reason":"NodeReady",
  "message":"Node node-a status is now: NodeReady","count":1,"lastTimestamp":"2026-11-18T07:00:00Z","source":{"component":"kubelet"}}`

func status(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"message": msg, "reason": reason, "code": code})
}

// writeList trả một trang của items theo limit / continue, như apiserver
// (https://kubernetes.io/docs/reference/using-api/api-concepts/#retrieving-large-results-sets-in-chunks).
func writeList(w http.ResponseWriter, r *http.Request, kind string, items []string) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset := 0
	if c := q.Get("continue"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || c == "ZXhwaXJlZA" { // "expired"
			status(w, http.StatusGone, "Expired", "The provided continue parameter is too old to display a consistent list result.")
			return
		}
		offset, _ = strconv.Atoi(string(raw))
	}
	end := len(items)
	if limit > 0 {
		end = min(offset+limit, len(items))
	}
	meta := `"resourceVersion":"100"`
	if end < len(items) {
		meta += fmt.Sprintf(`,"continue":%q,"remainingItemCount":%d`, base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end))), len(items)-end)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"kind":%q,"apiVersion":"v1","metadata":{%s},"items":[%s]}`, kind, meta, strings.Join(items[min(offset, end):end], ","))
}

func pods(ns string, n int, name func(i int) string) []string {
	items := make([]string, n)
	for i := range items {
		items[i] = podJSON(ns, name(i), "node-a")
	}
	return items
}

const leak = `rpc error: dial https://etcd-user:pw-secret@etcd.internal:2379/v3?token=q-secret failed, Authorization: Bearer bearer-in-message`

// fakeKube là một kube-apiserver tối thiểu: đúng các đường dẫn đọc mà integration
// dùng, theo định dạng của core/v1 và apps/v1.
func fakeKube(rec *recorder) http.Handler {
	long := strings.Repeat("n", 240)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/namespaces", func(w http.ResponseWriter, r *http.Request) {
		var items []string
		for _, n := range []string{"default", "kube-system", "prod", "staging"} {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":%q,"creationTimestamp":"2026-01-01T00:00:00Z","annotations":{"a":"annotation-secret"}},"status":{"phase":"Active"}}`, n))
		}
		writeList(w, r, "NamespaceList", items)
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		switch ns := r.PathValue("ns"); ns {
		case "prod":
			writeList(w, r, "PodList", pods(ns, 95, func(i int) string { return fmt.Sprintf("web-%03d", i) }))
		case "wide": // tên dài nhất hợp lý: trang lớn nhất mà apiserver giả lập trả được
			writeList(w, r, "PodList", pods(ns, 60, func(i int) string { return fmt.Sprintf("%s-%03d", long, i) }))
		case "odd": // một pod có tên node phình to sau khi escape, đứng trước một pod thật
			writeList(w, r, "PodList", []string{podJSON(ns, "huge", strings.Repeat("<", 253)), podJSON(ns, "real", "node-a")})
		case "unpaged": // thứ đứng trước apiserver bỏ qua limit
			q := r.URL.Query()
			q.Del("limit")
			r.URL.RawQuery = q.Encode()
			writeList(w, r, "PodList", pods(ns, 60, func(i int) string { return fmt.Sprintf("p-%03d", i) }))
		case "forbidden":
			status(w, http.StatusForbidden, "Forbidden", `pods is forbidden: User "system:serviceaccount:hq:hq-readonly" cannot list resource "pods" in API group "" in the namespace "forbidden"`)
		case "broken":
			status(w, http.StatusInternalServerError, "InternalError", leak)
		default:
			writeList(w, r, "PodList", []string{podJSON(ns, "api-0", "node-a")})
		}
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "missing" {
			status(w, http.StatusNotFound, "NotFound", `pods "missing" not found`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(podJSON(r.PathValue("ns"), r.PathValue("name"), "node-a")))
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{name}/log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		switch r.PathValue("name") {
		case "noisy": // ~330 KiB, mỗi byte "<" thành 6 byte trong JSON
			for i := range 1000 {
				_, _ = fmt.Fprintf(w, "2026-11-18T08:00:00.%09dZ line-%04d %s\n", i, i, strings.Repeat("<", 300))
			}
		case "endless":
			chunk := strings.Repeat("x", 1<<20)
			for range 10 {
				_, _ = w.Write([]byte(chunk))
			}
		case "starting":
			status(w, http.StatusBadRequest, "BadRequest", `container "app" in pod "starting" is waiting to start: ContainerCreating`)
		default:
			_, _ = w.Write([]byte("2026-11-18T08:00:00.000000001Z starting\n2026-11-18T08:00:01.000000001Z connecting with password=log-visible-marker\n2026-11-18T08:00:02.000000001Z listening on :8080\n"))
		}
	})
	mux.HandleFunc("GET /apis/apps/v1/namespaces/{ns}/deployments", func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		writeList(w, r, "DeploymentList", []string{deploymentJSON(ns, "api"), deploymentJSON(ns, "web"), deploymentJSON(ns, "worker")})
	})
	mux.HandleFunc("GET /apis/apps/v1/namespaces/{ns}/deployments/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(deploymentJSON(r.PathValue("ns"), r.PathValue("name"))))
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"EventList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[` + eventsItems + `]}`))
	})
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[` + nodesItems + `]}`))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		mux.ServeHTTP(w, r)
	})
}

var (
	testSigner = contract.TestSigner()
	testOrg    = uuid.New()
)

// headerTransport ký mỗi request như Gateway trên tuyến execution (contract.ExecutionTransport).
type headerTransport struct{ signer *signedctx.Signer }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return contract.ExecutionTransport(h.signer, kubernetes.Slug, testOrg, nil).RoundTrip(r)
}

// connect dựng server MCP trên client đã cho và nối một MCP client thật vào, với ngữ
// cảnh ký của org sở hữu.
func connect(t *testing.T, client *kubernetes.Client) *mcp.ClientSession {
	t.Helper()
	signer := testSigner
	srv := httptest.NewServer(kubernetes.Handler(client, signer.Verifier(), testOrg))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerTransport{signer}},
	}, nil)
	if err != nil {
		t.Fatalf("không kết nối được với ngữ cảnh hợp lệ: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func newFake(t *testing.T, namespaces ...string) (*kubernetes.Client, *recorder) {
	t.Helper()
	rec := &recorder{last: map[string]string{}}
	api := httptest.NewServer(fakeKube(rec))
	t.Cleanup(api.Close)
	client, err := kubernetes.NewClient(api.URL, kubernetes.Options{AllowHTTP: true, Namespaces: namespaces})
	if err != nil {
		t.Fatal(err)
	}
	return client, rec
}

// call gọi một tool và trả về kết quả cùng phần có cấu trúc dạng JSON (hoặc văn bản lỗi).
func call(t *testing.T, sess *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError || res.StructuredContent == nil {
		text := ""
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
		return res, text
	}
	b, _ := json.Marshal(res.StructuredContent)
	return res, string(b)
}

func decode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("kết quả không đọc được: %v: %.200s", err, out)
	}
	return v
}

func noSecrets(t *testing.T, what, out string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%s lộ %q: %.600s", what, s, out)
		}
	}
	for _, field := range []string{`"env"`, `"args"`, `"command"`, `"annotations"`, `"labels"`, `"managedFields"`, `"managed_fields"`, "secretName", "hostPath\":{"} {
		if strings.Contains(out, field) {
			t.Errorf("%s có trường không được phép %s", what, field)
		}
	}
}

type listOut struct {
	Namespaces       []map[string]any `json:"namespaces"`
	Pods             []map[string]any `json:"pods"`
	Deployments      []map[string]any `json:"deployments"`
	Events           []map[string]any `json:"events"`
	Nodes            []map[string]any `json:"nodes"`
	FromAllowlist    bool             `json:"from_allowlist"`
	NextCursor       string           `json:"next_cursor"`
	Remaining        int              `json:"remaining"`
	OversizedOmitted int              `json:"oversized_omitted"`
	FilteredOut      int              `json:"filtered_out"`
}

// Bộ test hợp đồng dùng chung của managed MCP server (mcp-platform §7).
func TestContract(t *testing.T) {
	client, rec := newFake(t)
	signer := testSigner
	maxBytes := kubernetes.MaxResultBytes
	contract.Run(t, contract.Server{
		Slug:           kubernetes.Slug,
		Handler:        kubernetes.Handler(client, signer.Verifier(), testOrg),
		Signer:         signer,
		OrgID:          testOrg,
		TargetRequests: rec.count,
		Tools: []contract.Tool{
			{Name: "list_namespaces", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{}, {"limit": 40}}},
			{Name: "list_pods", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{
				{"namespace": "prod"}, {"namespace": "prod", "limit": 40, "label_selector": "app=web,!canary,tier!=cache"},
				{"namespace": "wide", "limit": 40}}},
			{Name: "get_pod", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{"namespace": "default", "name": "api-0"}}},
			{Name: "get_pod_logs", MaxResultBytes: maxBytes, Samples: []map[string]any{
				{"namespace": "default", "pod": "api-0"},
				{"namespace": "default", "pod": "noisy", "container": "app", "tail_lines": 1000, "since_seconds": 3600, "previous": true}}},
			{Name: "list_deployments", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{"namespace": "prod"}, {"namespace": "prod", "limit": 40}}},
			{Name: "get_deployment", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{"namespace": "prod", "name": "web"}}},
			{Name: "list_events", MaxResultBytes: maxBytes, Samples: []map[string]any{
				{"namespace": "default"}, {"namespace": "default", "involved_kind": "Pod", "involved_name": "api-0", "type": "Warning", "limit": 40}}},
			{Name: "list_nodes", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{}, {"limit": 40}}},
		},
	})
}

func TestPods(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "list_pods", map[string]any{"namespace": "prod"})
	l := decode[listOut](t, out)
	if res.IsError || len(l.Pods) != 20 || l.NextCursor == "" || l.Remaining != 75 || l.Pods[0]["name"] != "web-000" ||
		l.Pods[0]["phase"] != "Running" || l.Pods[0]["waiting_reason"] != "CrashLoopBackOff" || l.Pods[0]["restarts"] != float64(8) ||
		l.Pods[0]["ready_containers"] != float64(1) || l.Pods[0]["containers"] != float64(4) || l.Pods[0]["owner_kind"] != "ReplicaSet" {
		t.Errorf("list_pods: %.600s", out)
	}
	noSecrets(t, "list_pods", out)
	if got := rec.lastQuery("/api/v1/namespaces/prod/pods"); got != "limit=20&timeoutSeconds=20" {
		t.Errorf("tham số gửi cho apiserver: %s", got)
	}

	// Phân trang bằng con trỏ đục đi hết được, không trùng, không sót.
	seen, token := map[string]bool{}, ""
	for range 10 {
		args := map[string]any{"namespace": "prod", "limit": 40}
		if token != "" {
			args["cursor"] = token
		}
		_, out = call(t, sess, "list_pods", args)
		l = decode[listOut](t, out)
		for _, p := range l.Pods {
			seen[p["name"].(string)] = true
		}
		if token = l.NextCursor; token == "" {
			break
		}
	}
	if len(seen) != 95 {
		t.Errorf("phân trang phải đi hết 95 pod, được %d", len(seen))
	}

	// Trang lớn nhất (40 pod, tên dài nhất) vẫn nằm trong trần.
	_, out = call(t, sess, "list_pods", map[string]any{"namespace": "wide", "limit": 40})
	if l := decode[listOut](t, out); len(l.Pods) != 40 || l.OversizedOmitted != 0 || len(out) > kubernetes.MaxResultBytes {
		t.Errorf("trang lớn: %d pod, %d bị bỏ, %d byte", len(l.Pods), l.OversizedOmitted, len(out))
	}
	// Một pod phình to bị bỏ và đếm, không che pod đứng sau nó.
	_, out = call(t, sess, "list_pods", map[string]any{"namespace": "odd"})
	if l := decode[listOut](t, out); l.OversizedOmitted != 1 || len(l.Pods) != 1 || l.Pods[0]["name"] != "real" {
		t.Errorf("pod phình to: %.300s", out)
	}

	// Label selector đã kiểm được chuyển nguyên vẹn (đã mã hoá) cho apiserver.
	call(t, sess, "list_pods", map[string]any{"namespace": "default", "label_selector": "app.kubernetes.io/name=web,!canary,tier!=cache"})
	if got := rec.lastQuery("/api/v1/namespaces/default/pods"); !strings.Contains(got, "labelSelector=app.kubernetes.io%2Fname%3Dweb%2C%21canary%2Ctier%21%3Dcache") {
		t.Errorf("labelSelector gửi cho apiserver: %s", got)
	}

	res, out = call(t, sess, "get_pod", map[string]any{"namespace": "default", "name": "api-0"})
	if res.IsError {
		t.Fatalf("get_pod: %s", out)
	}
	noSecrets(t, "get_pod", out)
	pod := decode[struct {
		Name          string `json:"name"`
		Phase         string `json:"phase"`
		QOSClass      string `json:"qos_class"`
		ContainerList []struct {
			Name            string            `json:"name"`
			Init            bool              `json:"init"`
			Image           string            `json:"image"`
			State           string            `json:"state"`
			Reason          string            `json:"reason"`
			Message         string            `json:"message"`
			RestartCount    int               `json:"restart_count"`
			ExitCode        *int              `json:"exit_code"`
			Ports           []string          `json:"ports"`
			Limits          map[string]string `json:"limits"`
			LastTermination *struct {
				Reason   string `json:"reason"`
				ExitCode int    `json:"exit_code"`
				Signal   int    `json:"signal"`
			} `json:"last_termination"`
		} `json:"container_list"`
		Volumes    []map[string]string `json:"volumes"`
		Conditions []map[string]string `json:"conditions"`
	}](t, out)
	if pod.Name != "api-0" || pod.Phase != "Running" || pod.QOSClass != "Burstable" || len(pod.ContainerList) != 5 || len(pod.Conditions) != 1 {
		t.Fatalf("get_pod: %s", out)
	}
	init, app, sidecar := pod.ContainerList[0], pod.ContainerList[1], pod.ContainerList[2]
	// Thông điệp chỉ có cho reason trong tập cho phép; terminated không bao giờ có thông điệp.
	cfg, runner := pod.ContainerList[3], pod.ContainerList[4]
	if cfg.Reason != "CreateContainerConfigError" || cfg.Message != "" || runner.Reason != "RunContainerError" || runner.Message != "" ||
		init.Message != "" || app.LastTermination.Signal != 9 || strings.Contains(out, `"message":"secret`) {
		t.Errorf("thông điệp do ứng dụng kiểm soát lọt ra: %s", out)
	}
	if !init.Init || init.State != "terminated" || init.ExitCode == nil || *init.ExitCode != 0 ||
		app.State != "waiting" || app.Reason != "CrashLoopBackOff" || app.RestartCount != 7 || app.Image != "registry.example/web:1.4.2" ||
		app.LastTermination == nil || app.LastTermination.Reason != "OOMKilled" || app.LastTermination.ExitCode != 137 ||
		strings.Join(app.Ports, ",") != "8080/TCP,9090/TCP" || app.Limits["memory"] != "512Mi" || len(app.Limits) != 2 ||
		!strings.Contains(app.Message, "back-off") || sidecar.State != "running" {
		t.Errorf("container của get_pod: %s", out)
	}
	// Volume: chỉ tên và loại; loại lạ thành "other".
	want := map[string]string{"creds": "secret", "host": "hostPath", "cache": "emptyDir", "strange": "other"}
	for _, v := range pod.Volumes {
		if want[v["name"]] != v["type"] || len(v) != 2 {
			t.Errorf("volume %v", v)
		}
	}
	if len(pod.Volumes) != 4 {
		t.Errorf("muốn 4 volume: %v", pod.Volumes)
	}

	if res, out := call(t, sess, "get_pod", map[string]any{"namespace": "default", "name": "missing"}); !res.IsError || !strings.Contains(out, "không tìm thấy") {
		t.Errorf("404 phải là lỗi tool: %s", out)
	}
}

func TestLogs(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0"})
	logs := decode[struct {
		Pod       string `json:"pod"`
		Logs      string `json:"logs"`
		Truncated bool   `json:"truncated"`
	}](t, out)
	if res.IsError || logs.Pod != "api-0" || logs.Truncated || !strings.HasSuffix(logs.Logs, "listening on :8080\n") {
		t.Errorf("get_pod_logs: %s", out)
	}
	// Ghi nhận hành vi: nội dung log KHÔNG được server lọc (việc che mẫu secret là của
	// Gateway; phân loại đề xuất của tool là sensitive). Test này tồn tại để việc đổi
	// hành vi đó là có chủ ý.
	if !strings.Contains(logs.Logs, "password=log-visible-marker") {
		t.Errorf("log phải được trả nguyên văn: %s", out)
	}
	if got := rec.lastQuery("/api/v1/namespaces/default/pods/api-0/log"); got != "tailLines=100&timestamps=true" {
		t.Errorf("tham số log mặc định: %s", got)
	}
	call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "container": "app", "tail_lines": 1000, "since_seconds": 600, "previous": true})
	if got := rec.lastQuery("/api/v1/namespaces/default/pods/api-0/log"); got != "container=app&previous=true&sinceSeconds=600&tailLines=1000&timestamps=true" {
		t.Errorf("tham số log: %s", got)
	}

	// Log lớn, phình 6 lần khi vào JSON: kết quả dưới trần, giữ phần MỚI nhất, bắt đầu ở đầu dòng.
	res, out = call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "noisy", "tail_lines": 1000})
	logs = decode[struct {
		Pod       string `json:"pod"`
		Logs      string `json:"logs"`
		Truncated bool   `json:"truncated"`
	}](t, out)
	if res.IsError || !logs.Truncated || len(out) > kubernetes.MaxResultBytes || len(out) < 32<<10 ||
		!strings.Contains(logs.Logs, "line-0999") || strings.Contains(logs.Logs, "line-0000") || !strings.HasPrefix(logs.Logs, "2026-11-18T") {
		t.Errorf("log lớn: %d byte, truncated=%v, đầu %.60q", len(out), logs.Truncated, logs.Logs)
	}

	// Luồng log dài quá trần đọc là lỗi tool ngắn, không phải phần CŨ của log.
	if res, out := call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "endless"}); !res.IsError || len(out) > 1024 {
		t.Errorf("luồng log quá dài: %d byte", len(out))
	}
	// Lỗi của apiserver cho log (container chưa chạy) tới được người gọi.
	if res, out := call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "starting"}); !res.IsError || !strings.Contains(out, "ContainerCreating") {
		t.Errorf("lỗi log: %s", out)
	}
}

func TestDeploymentsEventsNodesNamespaces(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "list_namespaces", map[string]any{"limit": 3})
	l := decode[listOut](t, out)
	if res.IsError || len(l.Namespaces) != 3 || l.NextCursor == "" || l.Remaining != 1 || l.FromAllowlist ||
		l.Namespaces[0]["name"] != "default" || l.Namespaces[0]["phase"] != "Active" {
		t.Errorf("list_namespaces: %s", out)
	}
	noSecrets(t, "list_namespaces", out)
	_, out = call(t, sess, "list_namespaces", map[string]any{"limit": 3, "cursor": l.NextCursor})
	if l := decode[listOut](t, out); len(l.Namespaces) != 1 || l.NextCursor != "" || l.Namespaces[0]["name"] != "staging" {
		t.Errorf("trang 2 của namespace: %s", out)
	}
	// Con trỏ hết hạn (410) là một lỗi tool rõ ràng.
	if res, out := call(t, sess, "list_namespaces", map[string]any{"cursor": "ZXhwaXJlZA"}); !res.IsError || !strings.Contains(out, "hết hạn") {
		t.Errorf("410: %s", out)
	}

	res, out = call(t, sess, "list_deployments", map[string]any{"namespace": "prod"})
	l = decode[listOut](t, out)
	if res.IsError || len(l.Deployments) != 3 || l.Deployments[1]["name"] != "web" || l.Deployments[1]["desired_replicas"] != float64(3) ||
		l.Deployments[1]["ready_replicas"] != float64(2) || l.Deployments[1]["unavailable_replicas"] != float64(1) ||
		l.Deployments[1]["generation"] != float64(4) || l.Deployments[1]["observed_generation"] != float64(3) {
		t.Errorf("list_deployments: %s", out)
	}
	noSecrets(t, "list_deployments", out)
	res, out = call(t, sess, "get_deployment", map[string]any{"namespace": "prod", "name": "web"})
	if res.IsError || !strings.Contains(out, `"strategy":"RollingUpdate"`) || !strings.Contains(out, "ProgressDeadlineExceeded") ||
		!strings.Contains(out, `"image":"registry.example/web:1.4.2"`) {
		t.Errorf("get_deployment: %s", out)
	}
	noSecrets(t, "get_deployment", out)

	res, out = call(t, sess, "list_events", map[string]any{"namespace": "default"})
	l = decode[listOut](t, out)
	if res.IsError || len(l.Events) != 4 || l.FilteredOut != 2 || l.Events[3]["involved_kind"] != "Node" || l.Events[0]["reason"] != "Failed" || l.Events[0]["count"] != float64(12) ||
		l.Events[0]["component"] != "kubelet" || l.Events[1]["reason"] != "ScalingReplicaSet" || l.Events[1]["count"] != float64(4) ||
		l.Events[1]["last_seen"] != "2026-11-18T08:20:00.000000Z" || l.Events[1]["component"] != "deployment-controller" ||
		l.Events[2]["reason"] != "Scheduled" {
		t.Errorf("list_events (gần nhất trước): %s", out)
	}
	noSecrets(t, "list_events", out)
	if !strings.Contains(out, "Failed to pull image") {
		t.Errorf("thông điệp event phải còn phần không nhạy cảm: %s", out)
	}
	call(t, sess, "list_events", map[string]any{"namespace": "default", "involved_kind": "Pod", "involved_name": "api-0", "type": "Warning"})
	if got := rec.lastQuery("/api/v1/namespaces/default/events"); !strings.Contains(got, "fieldSelector=involvedObject.kind%3DPod%2CinvolvedObject.name%3Dapi-0%2Ctype%3DWarning") {
		t.Errorf("fieldSelector gửi cho apiserver: %s", got)
	}

	res, out = call(t, sess, "list_nodes", map[string]any{})
	l = decode[listOut](t, out)
	if res.IsError || len(l.Nodes) != 2 {
		t.Fatalf("list_nodes: %s", out)
	}
	noSecrets(t, "list_nodes", out)
	a, b := l.Nodes[0], l.Nodes[1]
	if a["name"] != "node-a" || a["ready"] != "True" || a["unschedulable"] != true || fmt.Sprint(a["problems"]) != "[DiskPressure]" ||
		fmt.Sprint(a["roles"]) != "[control-plane]" || fmt.Sprint(a["taints"]) != "[dedicated:NoSchedule]" || a["zone"] != "zone-1" ||
		a["instance_type"] != "m5.large" || a["kubelet_version"] != "v1.34.1" || a["internal_ip"] != "192.168.0.10" ||
		fmt.Sprint(a["allocatable"]) != "map[cpu:3900m memory:15Gi]" || b["ready"] != "Unknown" || fmt.Sprint(b["roles"]) != "[worker]" {
		t.Errorf("list_nodes: %s", out)
	}
}

// Allowlist namespace: mọi tool bị giới hạn trong đó, và lời từ chối không tới apiserver.
func TestNamespaceAllowlist(t *testing.T) {
	client, rec := newFake(t, "prod", "default")
	sess := connect(t, client)

	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"list_pods":        {"list_pods", map[string]any{"namespace": "kube-system"}},
		"get_pod":          {"get_pod", map[string]any{"namespace": "kube-system", "name": "etcd-0"}},
		"get_pod_logs":     {"get_pod_logs", map[string]any{"namespace": "kube-system", "pod": "etcd-0"}},
		"list_deployments": {"list_deployments", map[string]any{"namespace": "staging"}},
		"get_deployment":   {"get_deployment", map[string]any{"namespace": "staging", "name": "web"}},
		"list_events":      {"list_events", map[string]any{"namespace": "kube-system"}},
		"list_nodes":       {"list_nodes", map[string]any{}},
		"event của Node":   {"list_events", map[string]any{"namespace": "default", "involved_kind": "Node"}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s ngoài allowlist phải bị từ chối: %.200s", name, out)
		}
	}
	// list_namespaces trả đúng allowlist, không hỏi cluster.
	res, out := call(t, sess, "list_namespaces", map[string]any{})
	l := decode[listOut](t, out)
	if res.IsError || !l.FromAllowlist || len(l.Namespaces) != 2 || l.Namespaces[0]["name"] != "default" || l.Namespaces[1]["name"] != "prod" {
		t.Errorf("list_namespaces với allowlist: %s", out)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("request ngoài allowlist đã tới apiserver: %v", rec.requests())
	}

	// Event: với allowlist, event của Secret, ConfigMap VÀ Node đều bị bỏ và đếm.
	_, out = call(t, sess, "list_events", map[string]any{"namespace": "default"})
	l = decode[listOut](t, out)
	if len(l.Events) != 3 || l.FilteredOut != 3 || strings.Contains(out, "NodeReady") || strings.Contains(out, `"Node"`) {
		t.Errorf("list_events với allowlist: %s", out)
	}
	noSecrets(t, "list_events với allowlist", out)

	// Trong allowlist thì chạy bình thường.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"list_pods", map[string]any{"namespace": "prod"}},
		{"get_pod", map[string]any{"namespace": "default", "name": "api-0"}},
		{"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0"}},
		{"list_deployments", map[string]any{"namespace": "prod"}},
		{"list_events", map[string]any{"namespace": "default"}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); res.IsError {
			t.Errorf("%s trong allowlist: %s", tc.tool, out)
		}
	}
	for _, r := range rec.requests() {
		if !strings.Contains(r, "/namespaces/prod/") && !strings.Contains(r, "/namespaces/default/") {
			t.Errorf("request ngoài allowlist: %s", r)
		}
	}

	// Cấu hình phạm vi: bắt buộc và tường minh. Trắng bị từ chối; "*" đứng một mình là mọi
	// namespace; chuỗi hỏng bị từ chối chứ không thành "mọi namespace".
	if got, all, err := kubernetes.ParseNamespaces(" * "); err != nil || !all || got != nil {
		t.Errorf("*: %v %v %v", got, all, err)
	}
	if got, all, err := kubernetes.ParseNamespaces(" prod, default ,prod"); err != nil || all || strings.Join(got, ",") != "default,prod" {
		t.Errorf("allowlist hợp lệ: %v %v %v", got, all, err)
	}
	for _, bad := range []string{"", "  ", ",", "prod,,default", "Prod", "prod;default", "a/b", "**", "prod,*", "*,prod", strings.Repeat("a", 64)} {
		if got, all, err := kubernetes.ParseNamespaces(bad); err == nil || all || got != nil {
			t.Errorf("phạm vi %q phải bị từ chối", bad)
		}
	}
}

// Trần request đồng thời tới apiserver: request vượt trần chờ có hạn rồi nhận lỗi "đang
// bận", không tới apiserver.
func TestConcurrencyCap(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 4)
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"kind":"NodeList","metadata":{},"items":[]}`))
	}))
	t.Cleanup(api.Close)
	client, err := kubernetes.NewClient(api.URL, kubernetes.Options{AllowHTTP: true, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, _, err := client.Nodes(context.Background(), 0, ""); err != nil {
				t.Errorf("request trong trần: %v", err)
			}
		})
	}
	<-entered
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := client.Nodes(ctx, 0, ""); err == nil || !strings.Contains(err.Error(), "bận") {
		t.Errorf("request vượt trần phải nhận lỗi đang bận: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("request vượt trần đã tới apiserver: %d", n)
	}
	close(release)
	wg.Wait()
	// Suất được trả lại sau khi request xong.
	if _, _, err := client.Nodes(context.Background(), 0, ""); err != nil {
		t.Errorf("sau khi có suất trống: %v", err)
	}
	if _, err := kubernetes.NewClient(api.URL, kubernetes.Options{AllowHTTP: true, MaxConcurrent: 65}); err == nil {
		t.Error("MaxConcurrent ngoài khoảng phải bị từ chối")
	}
}

// Đầu vào ngoài giới hạn là lỗi tool và không tới apiserver: qua schema (Gateway và MCP
// SDK), và một lần nữa ở Client (phòng khi schema bị nới).
func TestInvalidInput(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"namespace thoát đường dẫn": {"list_pods", map[string]any{"namespace": "../kube-system"}},
		"namespace có gạch chéo":    {"list_pods", map[string]any{"namespace": "a/b"}},
		"namespace đã mã hoá":       {"list_pods", map[string]any{"namespace": "a%2Fb"}},
		"namespace viết hoa":        {"list_pods", map[string]any{"namespace": "Prod"}},
		"namespace quá dài":         {"list_pods", map[string]any{"namespace": strings.Repeat("a", 64)}},
		"thiếu namespace":           {"list_pods", map[string]any{}},
		"tên là ..":                 {"get_pod", map[string]any{"namespace": "default", "name": ".."}},
		"tên thoát đường dẫn":       {"get_pod", map[string]any{"namespace": "default", "name": "a/../../secrets/x"}},
		"tên có query":              {"get_pod", map[string]any{"namespace": "default", "name": "a?watch=true"}},
		"tên bắt đầu bằng gạch":     {"get_deployment", map[string]any{"namespace": "default", "name": "-x"}},
		"container viết hoa":        {"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "container": "App"}},
		"tail_lines vượt trần":      {"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "tail_lines": 1001}},
		"since_seconds âm":          {"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "since_seconds": -1}},
		"since_seconds vượt trần":   {"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "since_seconds": 604801}},
		"log: trường follow":        {"get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0", "follow": true}},
		"selector dạng tập hợp":     {"list_pods", map[string]any{"namespace": "default", "label_selector": "app in (a,b)"}},
		"selector chèn tham số":     {"list_pods", map[string]any{"namespace": "default", "label_selector": "app=web&fieldSelector=x"}},
		"selector thiếu key":        {"list_pods", map[string]any{"namespace": "default", "label_selector": "=web"}},
		"selector hai dấu bằng":     {"list_pods", map[string]any{"namespace": "default", "label_selector": "a=b=c"}},
		"selector !key=value":       {"list_pods", map[string]any{"namespace": "default", "label_selector": "!a=b"}},
		"selector điều kiện rỗng":   {"list_pods", map[string]any{"namespace": "default", "label_selector": "a=b,,c=d"}},
		"selector quá 8 điều kiện":  {"list_pods", map[string]any{"namespace": "default", "label_selector": "a,b,c,d,e,f,g,h,i"}},
		"selector giá trị quá dài":  {"list_pods", map[string]any{"namespace": "default", "label_selector": "a=" + strings.Repeat("v", 64)}},
		"limit vượt trần":           {"list_pods", map[string]any{"namespace": "default", "limit": 41}},
		"limit âm":                  {"list_nodes", map[string]any{"limit": -1}},
		"cursor có khoảng trắng":    {"list_nodes", map[string]any{"cursor": "a b"}},
		"cursor chèn tham số":       {"list_nodes", map[string]any{"cursor": "x&limit=5000"}},
		"involved_name thiếu kind":  {"list_events", map[string]any{"namespace": "default", "involved_name": "api-0"}},
		"involved_kind ngoài enum":  {"list_events", map[string]any{"namespace": "default", "involved_kind": "Secret"}},
		"involved_name chèn bộ lọc": {"list_events", map[string]any{"namespace": "default", "involved_kind": "Pod", "involved_name": "a,type=Normal"}},
		"type ngoài enum":           {"list_events", map[string]any{"namespace": "default", "type": "Error"}},
		"trường lạ":                 {"list_events", map[string]any{"namespace": "default", "field_selector": "type=Normal"}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}

	ctx := context.Background()
	for name, err := range map[string]error{
		"Pods namespace":     second(client.Pods(ctx, "../x", "", 0, "")),
		"Pods selector":      second(client.Pods(ctx, "default", "app in (a)", 0, "")),
		"Pods selector 2":    second(client.Pods(ctx, "default", "a=b=c", 0, "")),
		"Pods limit":         second(client.Pods(ctx, "default", "", 41, "")),
		"Pods token":         second(client.Pods(ctx, "default", "", 0, "a&b")),
		"Pod name":           second2(client.Pod(ctx, "default", "a/b")),
		"Pod dotdot":         second2(client.Pod(ctx, "default", "..")),
		"Logs container":     third(client.Logs(ctx, "default", "api-0", "a/b", 0, 0, false)),
		"Logs tail":          third(client.Logs(ctx, "default", "api-0", "", 1001, 0, false)),
		"Logs pod":           third(client.Logs(ctx, "default", "a%2Fb", "", 0, 0, false)),
		"Deployment name":    second2(client.Deployment(ctx, "default", "A")),
		"Deployments ns":     second(client.Deployments(ctx, "a.b", 0, "")),
		"Events kind":        second(client.Events(ctx, "default", "Secret", "", "", 0, "")),
		"Events name":        second(client.Events(ctx, "default", "Pod", "a,b", "", 0, "")),
		"Events name-nokind": second(client.Events(ctx, "default", "", "api-0", "", 0, "")),
		"Events type":        second(client.Events(ctx, "default", "", "", "Error", 0, "")),
	} {
		if err == nil {
			t.Errorf("Client %s: phải bị từ chối", name)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("đầu vào xấu đã tới apiserver: %v", rec.requests())
	}
}

func second[T any](_ T, _ kubernetes.Page, err error) error { return err }
func second2[T any](_ T, err error) error                   { return err }
func third(_ string, _ bool, err error) error               { return err }

// Server chỉ đọc: đúng tám tool, chỉ GET, chỉ tới các đường dẫn đọc đã định; không
// đường dẫn nào chạm secrets, configmaps, exec, attach, proxy hay portforward.
func TestReadOnlySurface(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s thiếu readOnlyHint", tool.Name)
		}
	}
	if got := strings.Join(names, ","); got != "get_deployment,get_pod,get_pod_logs,list_deployments,list_events,list_namespaces,list_nodes,list_pods" {
		t.Errorf("danh sách tool: %s", got)
	}
	call(t, sess, "list_namespaces", map[string]any{})
	call(t, sess, "list_pods", map[string]any{"namespace": "prod"})
	call(t, sess, "get_pod", map[string]any{"namespace": "default", "name": "api-0"})
	call(t, sess, "get_pod_logs", map[string]any{"namespace": "default", "pod": "api-0"})
	call(t, sess, "list_deployments", map[string]any{"namespace": "prod"})
	call(t, sess, "get_deployment", map[string]any{"namespace": "prod", "name": "web"})
	call(t, sess, "list_events", map[string]any{"namespace": "default"})
	call(t, sess, "list_nodes", map[string]any{})
	const seg = `[a-z0-9.-]+`
	allowed := regexp.MustCompile(`^GET (/api/v1/namespaces|/api/v1/nodes|/api/v1/namespaces/` + seg + `/pods(/` + seg + `(/log)?)?|/api/v1/namespaces/` + seg +
		`/events|/apis/apps/v1/namespaces/` + seg + `/deployments(/` + seg + `)?)$`)
	reqs := rec.requests()
	if len(reqs) != 8 {
		t.Errorf("muốn 8 request, có %v", reqs)
	}
	for _, r := range reqs {
		if !allowed.MatchString(r) {
			t.Errorf("server gọi apiserver ngoài phạm vi đọc: %s", r)
		}
	}
}

// Bearer token là cấu hình phía server: được gửi cho apiserver qua TLS đã xác minh bằng
// CA cấu hình, không theo redirect sang nơi khác, không xuất hiện trong kết quả hay lỗi.
func TestCredentialStaysServerSide(t *testing.T) {
	const token = "kube-sa-token-secret"
	var leaked sync.Map
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store("hit", r.Header.Get("Authorization"))
	}))
	t.Cleanup(elsewhere.Close)

	rec := &recorder{last: map[string]string{}}
	fake := fakeKube(rec)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			rec.add(r)
			http.Redirect(w, r, elsewhere.URL+"/api/v1/nodes", http.StatusFound)
		case "/api/v1/namespaces/denied/pods":
			rec.add(r)
			status(w, http.StatusUnauthorized, "Unauthorized", "token "+token+" is not valid")
		case "/api/v1/namespaces/echo/events":
			rec.add(r)
			_, _ = w.Write([]byte(`{"metadata":{},"items":[{"involvedObject":{"kind":"Pod","name":"p"},"type":"Warning","reason":"Failed","message":"event said ` + token + ` plainly","lastTimestamp":"2026-11-18T08:00:00Z"}]}`))
		case "/api/v1/namespaces/echo/pods/p":
			rec.add(r)
			_, _ = w.Write([]byte(`{"metadata":{"name":"p","namespace":"echo"},"spec":{"containers":[{"name":"app","image":"i"}]},"status":{"phase":"Pending",
				"conditions":[{"type":"Ready","status":"False","message":"condition said ` + token + ` plainly"}],
				"containerStatuses":[{"name":"app","state":{"waiting":{"reason":"ErrImagePull","message":"kubelet said ` + token + ` plainly"}}}]}}`))
		case "/apis/apps/v1/namespaces/echo/deployments/d":
			rec.add(r)
			_, _ = w.Write([]byte(`{"metadata":{"name":"d","namespace":"echo"},"spec":{},"status":{"conditions":[{"type":"ReplicaFailure","status":"True","message":"webhook said ` + token + ` plainly"}]}}`))
		case "/api/v1/namespaces/echo/pods":
			rec.add(r)
			status(w, http.StatusInternalServerError, "InternalError", "upstream rejected header Authorization: Bearer "+token+" and raw "+token)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(api.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})

	client, err := kubernetes.NewClient(api.URL, kubernetes.Options{BearerToken: token + "\n", CAPEM: ca})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)

	res, out := call(t, sess, "list_namespaces", map[string]any{})
	if res.IsError {
		t.Fatalf("gọi qua https kèm token và CA: %s", out)
	}
	rec.mu.Lock()
	sent := rec.auth[0]
	rec.mu.Unlock()
	if sent != "Bearer "+token {
		t.Errorf("header Authorization gửi cho apiserver: %q", sent)
	}

	res, redirected := call(t, sess, "list_nodes", map[string]any{})
	if !res.IsError {
		t.Errorf("redirect phải là lỗi tool: %s", redirected)
	}
	if v, ok := leaked.Load("hit"); ok {
		t.Errorf("server đã theo redirect sang địa chỉ khác (Authorization: %q)", v)
	}
	res, denied := call(t, sess, "list_pods", map[string]any{"namespace": "denied"})
	if !res.IsError {
		t.Errorf("401 của apiserver phải là lỗi tool: %s", denied)
	}
	res, echoed := call(t, sess, "list_pods", map[string]any{"namespace": "echo"})
	if !res.IsError || !strings.Contains(echoed, "InternalError") {
		t.Errorf("500 của apiserver phải là lỗi tool kèm lý do: %s", echoed)
	}
	// 403 (RBAC) không đưa tên service account cho model.
	res, forbidden := call(t, sess, "list_pods", map[string]any{"namespace": "forbidden"})
	if !res.IsError || strings.Contains(forbidden, "serviceaccount") {
		t.Errorf("403: %s", forbidden)
	}
	// Thông báo lỗi của apiserver chép URL nội bộ kèm credential: bị che.
	res, broken := call(t, sess, "list_pods", map[string]any{"namespace": "broken"})
	if !res.IsError || !strings.Contains(broken, "rpc error") {
		t.Errorf("500 kèm thông báo: %s", broken)
	}
	// Token của chính server bị chép vào văn bản tự do (không có hình thù nào để nhận ra):
	// vẫn bị che trên đường event, condition và thông điệp của container.
	_, evOut := call(t, sess, "list_events", map[string]any{"namespace": "echo"})
	_, podOut := call(t, sess, "get_pod", map[string]any{"namespace": "echo", "name": "p"})
	_, depOut := call(t, sess, "get_deployment", map[string]any{"namespace": "echo", "name": "d"})
	if !strings.Contains(evOut, "event said") || !strings.Contains(podOut, "condition said") || !strings.Contains(podOut, "kubelet said") ||
		!strings.Contains(depOut, "webhook said") {
		t.Errorf("thông điệp phải còn phần không nhạy cảm: %s | %s | %s", evOut, podOut, depOut)
	}
	for _, text := range []string{out, redirected, denied, echoed, forbidden, broken, evOut, podOut, depOut} {
		if strings.Contains(text, token) || strings.Contains(text, elsewhere.URL) || strings.Contains(text, api.URL) ||
			strings.Contains(text, "pw-secret") || strings.Contains(text, "q-secret") || strings.Contains(text, "bearer-in-message") {
			t.Errorf("kết quả tool lộ token hoặc địa chỉ nội bộ: %s", text)
		}
	}

	// CA sai: TLS không được xác minh thì không gọi được, và không có đường tắt.
	other := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(other.Close)
	wrongCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: elsewhereCert(t)})
	strict, err := kubernetes.NewClient(other.URL, kubernetes.Options{BearerToken: token, CAPEM: wrongCA})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := strict.Namespaces(context.Background(), 0, ""); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("chứng chỉ không khớp CA phải là lỗi chung chung: %v", err)
	}

	// Cấu hình không an toàn bị từ chối lúc khởi động.
	for name, tc := range map[string]struct {
		url  string
		opts kubernetes.Options
	}{
		"http ngoài local":      {"http://kube:8080", kubernetes.Options{}},
		"token qua http":        {"http://kube:8080", kubernetes.Options{AllowHTTP: true, BearerToken: token}},
		"https thiếu token":     {"https://kube:6443", kubernetes.Options{}},
		"credential trong URL":  {"https://user:pw@kube:6443", kubernetes.Options{BearerToken: token}},
		"URL có query":          {"https://kube:6443/?token=x", kubernetes.Options{BearerToken: token}},
		"không phải http(s)":    {"file:///etc/passwd", kubernetes.Options{BearerToken: token}},
		"rỗng":                  {"", kubernetes.Options{BearerToken: token}},
		"token chứa xuống dòng": {"https://kube:6443", kubernetes.Options{BearerToken: "a\r\nX-Injected: 1"}},
		"CA không phải PEM":     {"https://kube:6443", kubernetes.Options{BearerToken: token, CAPEM: []byte("not a cert")}},
		"allowlist hỏng":        {"https://kube:6443", kubernetes.Options{BearerToken: token, Namespaces: []string{"a/b"}}},
	} {
		if _, err := kubernetes.NewClient(tc.url, tc.opts); err == nil {
			t.Errorf("%s: NewClient phải từ chối", name)
		} else if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: lỗi cấu hình lộ credential: %v", name, err)
		}
	}
}

// elsewhereCert sinh một chứng chỉ CA tự ký không liên quan tới máy chủ đang gọi.
func elsewhereCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// Apiserver chết hoặc trả rác: lỗi tool chung chung, không lộ địa chỉ nội bộ.
func TestUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	client, err := kubernetes.NewClient(dead.URL, kubernetes.Options{AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)
	res, out := call(t, sess, "list_namespaces", map[string]any{})
	if !res.IsError || strings.Contains(out, "127.0.0.1") {
		t.Errorf("apiserver không tới được: %s", out)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodes"):
			_, _ = w.Write([]byte(strings.Repeat("x", 9<<20)))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		default:
			_, _ = w.Write([]byte("<html>login</html>"))
		}
	}))
	t.Cleanup(garbage.Close)
	client, _ = kubernetes.NewClient(garbage.URL+"/k8s/clusters/c-1/", kubernetes.Options{AllowHTTP: true})
	sess = connect(t, client)
	if res, out := call(t, sess, "list_namespaces", map[string]any{}); !res.IsError {
		t.Errorf("phản hồi không phải JSON phải là lỗi tool: %s", out)
	}
	if res, out := call(t, sess, "list_nodes", map[string]any{}); !res.IsError || len(out) > 1024 {
		t.Errorf("phản hồi quá lớn phải là lỗi tool ngắn: %d byte", len(out))
	}
	if res, out := call(t, sess, "list_events", map[string]any{"namespace": "default"}); !res.IsError || !strings.Contains(out, "502") || strings.Contains(out, "html") {
		t.Errorf("lỗi không phải Status: %s", out)
	}

	// Thứ đứng trước apiserver bỏ qua limit: từ chối thay vì trả kết quả vượt trần hoặc
	// lặng lẽ mất phần tử.
	client, _ = newFake(t)
	sess = connect(t, client)
	if res, out := call(t, sess, "list_pods", map[string]any{"namespace": "unpaged"}); !res.IsError {
		t.Errorf("apiserver bỏ qua limit phải là lỗi tool: %.200s", out)
	}
}
