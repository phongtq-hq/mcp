// Package docker là managed MCP server chỉ đọc cho Docker: liệt kê container, xem
// trạng thái và đọc log. Đây là integration đầu tiên của platform và là use case
// dẫn đường (DevOps Agent chỉ đọc trên Docker).
//
// Server này KHÔNG có tool ghi và không có tool chạy lệnh. Nó nên nói chuyện với
// Docker qua một proxy chỉ cho phép API đọc container (deployments/compose), không
// gắn thẳng Docker socket: ai gọi được toàn bộ Docker API là có quyền tương đương
// root trên máy.
package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxContainers = 200
	maxAPIBody    = 8 << 20
	maxLogBytes   = 256 << 10
	// maxLogJSONBytes là trần của log sau khi tuần tự hoá thành JSON: dưới trần kết quả
	// mặc định 64 KiB của Registry, chừa chỗ cho phần còn lại của kết quả.
	maxLogJSONBytes  = 60 << 10
	maxLogTail       = 1000
	defaultLogTail   = 100
	defaultAPITimout = 20 * time.Second
)

// Tên hoặc ID container: không chứa "/" hay "..", nên không thoát được khỏi đường
// dẫn API /containers/{name}/….
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ErrNotFound báo container không tồn tại.
var ErrNotFound = errors.New("container không tồn tại")

var errUnavailable = errors.New("không gọi được Docker API")

// Client gọi Docker Engine API. Chỉ dùng các endpoint GET đọc container.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient tạo client. apiURL là địa chỉ HTTP của Docker API (thường là proxy chỉ
// đọc, ví dụ http://docker-proxy:2375). hc nil thì dùng client mặc định có timeout.
func NewClient(apiURL string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("docker: địa chỉ API phải là URL http(s)")
	}
	if hc == nil {
		hc = &http.Client{Timeout: defaultAPITimout}
	}
	return &Client{base: strings.TrimRight(apiURL, "/"), hc: hc}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// Lỗi của net/http chứa URL và địa chỉ nội bộ; nó đi vào log, không đi vào
		// kết quả tool.
		slog.WarnContext(ctx, "docker api: request lỗi", "error", err)
		return nil, errUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		slog.WarnContext(ctx, "docker api: đọc phản hồi lỗi", "error", err)
		return nil, errUnavailable
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("docker api: status %d", resp.StatusCode)
	}
	return body, nil
}

// Container là thông tin tóm tắt của một container.
type Container struct {
	ID      string   `json:"id" jsonschema:"12 ký tự đầu của ID"`
	Names   []string `json:"names"`
	Image   string   `json:"image"`
	State   string   `json:"state" jsonschema:"running, restarting, exited, paused, created, dead"`
	Status  string   `json:"status" jsonschema:"mô tả trạng thái do Docker tạo, ví dụ 'Restarting (1) 4 seconds ago'"`
	Created string   `json:"created" jsonschema:"thời điểm tạo, RFC 3339"`
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ListContainers liệt kê container. all = false chỉ gồm container đang chạy.
func (c *Client) ListContainers(ctx context.Context, all bool, limit int) (containers []Container, truncated bool, err error) {
	if limit <= 0 || limit > maxContainers {
		limit = maxContainers
	}
	q := url.Values{"all": {strconv.FormatBool(all)}, "limit": {strconv.Itoa(limit + 1)}}
	body, err := c.get(ctx, "/containers/json", q)
	if err != nil {
		return nil, false, err
	}
	var raw []struct {
		ID      string   `json:"Id"`
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
		Created int64    `json:"Created"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, fmt.Errorf("docker api: phản hồi không đọc được: %w", err)
	}
	if len(raw) > limit {
		raw, truncated = raw[:limit], true
	}
	containers = make([]Container, len(raw))
	for i, r := range raw {
		names := make([]string, len(r.Names))
		for j, n := range r.Names {
			names[j] = strings.TrimPrefix(n, "/")
		}
		containers[i] = Container{ID: shortID(r.ID), Names: names, Image: r.Image, State: r.State, Status: r.Status,
			Created: time.Unix(r.Created, 0).UTC().Format(time.RFC3339)}
	}
	return containers, truncated, nil
}

// Detail là thông tin chi tiết của một container, đã lược bỏ phần nhạy cảm.
//
// Cố ý KHÔNG gồm biến môi trường, label, lệnh khởi chạy và đường dẫn mount trên
// host: đó là nơi thường chứa mật khẩu, token và chuỗi kết nối.
type Detail struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Created       string            `json:"created"`
	Status        string            `json:"status" jsonschema:"running, restarting, exited, paused, created, dead"`
	Running       bool              `json:"running"`
	Restarting    bool              `json:"restarting"`
	OOMKilled     bool              `json:"oom_killed"`
	ExitCode      int               `json:"exit_code"`
	Error         string            `json:"error,omitempty"`
	StartedAt     string            `json:"started_at,omitempty"`
	FinishedAt    string            `json:"finished_at,omitempty"`
	RestartCount  int               `json:"restart_count"`
	Health        string            `json:"health,omitempty" jsonschema:"healthy, unhealthy, starting; rỗng nếu container không có healthcheck"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	Ports         map[string]string `json:"ports,omitempty" jsonschema:"cổng container → cổng host"`
	Networks      []string          `json:"networks,omitempty"`
}

// Inspect trả về thông tin chi tiết của một container theo tên hoặc ID.
func (c *Client) Inspect(ctx context.Context, name string) (Detail, error) {
	if !nameRe.MatchString(name) {
		return Detail{}, errors.New("tên container không hợp lệ")
	}
	body, err := c.get(ctx, "/containers/"+name+"/json", nil)
	if err != nil {
		return Detail{}, err
	}
	var raw struct {
		ID           string `json:"Id"`
		Name         string `json:"Name"`
		Created      string `json:"Created"`
		RestartCount int    `json:"RestartCount"`
		Config       struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Status     string `json:"Status"`
			Running    bool   `json:"Running"`
			Restarting bool   `json:"Restarting"`
			OOMKilled  bool   `json:"OOMKilled"`
			ExitCode   int    `json:"ExitCode"`
			Error      string `json:"Error"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
			Health     *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		HostConfig struct {
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Detail{}, fmt.Errorf("docker api: phản hồi không đọc được: %w", err)
	}
	d := Detail{
		ID: shortID(raw.ID), Name: strings.TrimPrefix(raw.Name, "/"), Image: raw.Config.Image, Created: raw.Created,
		Status: raw.State.Status, Running: raw.State.Running, Restarting: raw.State.Restarting,
		OOMKilled: raw.State.OOMKilled, ExitCode: raw.State.ExitCode, Error: raw.State.Error,
		StartedAt: raw.State.StartedAt, FinishedAt: raw.State.FinishedAt, RestartCount: raw.RestartCount,
		RestartPolicy: raw.HostConfig.RestartPolicy.Name,
	}
	if raw.State.Health != nil {
		d.Health = raw.State.Health.Status
	}
	for port, bindings := range raw.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		if d.Ports == nil {
			d.Ports = map[string]string{}
		}
		d.Ports[port] = bindings[0].HostIP + ":" + bindings[0].HostPort
	}
	for n := range raw.NetworkSettings.Networks {
		d.Networks = append(d.Networks, n)
	}
	return d, nil
}

// Logs đọc phần cuối log (stdout + stderr) của một container. tail là số dòng;
// since > 0 chỉ lấy log trong khoảng thời gian đó trở lại. Kết quả bị giới hạn ở
// maxLogBytes, giữ phần mới nhất.
func (c *Client) Logs(ctx context.Context, name string, tail int, since time.Duration) (text string, truncated bool, err error) {
	if !nameRe.MatchString(name) {
		return "", false, errors.New("tên container không hợp lệ")
	}
	if tail <= 0 {
		tail = defaultLogTail
	}
	if tail > maxLogTail {
		tail = maxLogTail
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(tail)}}
	if since > 0 {
		q.Set("since", strconv.FormatInt(time.Now().Add(-since).Unix(), 10))
	}
	body, err := c.get(ctx, "/containers/"+name+"/logs", q)
	if err != nil {
		return "", false, err
	}
	out := demux(body)
	if len(out) > maxLogBytes {
		out, truncated = out[len(out)-maxLogBytes:], true
	}
	text = strings.ToValidUTF8(string(out), "�")
	// Trần là kích thước ĐÃ TUẦN TỰ HOÁ: ký tự điều khiển và < > & phình tới 6 lần khi vào
	// JSON, và Gateway cắt kết quả vượt trần của nó ở giữa chừng (mất cả cờ truncated, và
	// giữ phần CŨ nhất). Ở đây giữ phần mới nhất. Vòng lặp có trần: mỗi lượt bỏ ít nhất
	// phần vượt.
	for range 32 {
		n := jsonLen(text)
		if n <= maxLogJSONBytes {
			break
		}
		drop := min(len(text)*(n-maxLogJSONBytes)/n+1, len(text))
		text, truncated = strings.ToValidUTF8(text[drop:], ""), true
	}
	if truncated {
		// Bắt đầu ở đầu một dòng trọn vẹn.
		if i := strings.IndexByte(text, '\n'); i >= 0 && i < len(text)-1 {
			text = text[i+1:]
		}
	}
	return text, truncated || len(body) >= maxAPIBody, nil
}

// demux tách luồng log ghép của Docker (mỗi khung: 1 byte loại luồng, 3 byte 0,
// 4 byte độ dài big-endian, rồi dữ liệu). Container chạy với TTY trả log thô,
// không có khung: khi đó trả nguyên dữ liệu.
func demux(b []byte) []byte {
	var out bytes.Buffer
	rest := b
	for len(rest) > 0 {
		if len(rest) < 8 || rest[0] > 2 || rest[1] != 0 || rest[2] != 0 || rest[3] != 0 {
			return b // không phải định dạng ghép
		}
		n := int(binary.BigEndian.Uint32(rest[4:8]))
		if n > len(rest)-8 {
			n = len(rest) - 8 // khung cuối bị cắt do giới hạn kích thước
		}
		out.Write(rest[8 : 8+n])
		rest = rest[8+n:]
	}
	return out.Bytes()
}

// jsonLen là số byte của s khi nằm trong một tài liệu JSON.
func jsonLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b)
}
