// Package registry là MCP Registry: server, version (ảnh chụp bất biến của một lần
// discovery) và tool kèm thuộc tính chính sách (mcp-platform §5).
//
// Định nghĩa của một tool (tên, mô tả, input schema) được đưa vào prompt của model,
// nên là một kênh tiêm lệnh. Registry ghim hash của từng định nghĩa; định nghĩa
// thay đổi thì tạo version mới chờ duyệt, và tool bị đổi bị tạm khoá cho tới khi
// version mới được duyệt.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxTools         = 200
	maxDescription   = 8 << 10
	maxSchemaBytes   = 64 << 10
	discoveryTimeout = 30 * time.Second
)

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DiscoveredTool là định nghĩa một tool như server trả về, đã chuẩn hoá.
type DiscoveredTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// ReadOnlyHint là annotation của server. Chỉ là gợi ý: với server ngoài nó
	// không được tin; với managed server (code của chính platform) nó dùng để
	// phân loại ban đầu.
	ReadOnlyHint bool   `json:"readOnlyHint"`
	Hash         []byte `json:"-"`
}

// Snapshot là kết quả của một lần discovery.
type Snapshot struct {
	ProtocolVersion string
	ReportedVersion string
	Tools           []DiscoveredTool
	Raw             json.RawMessage // danh sách tool đã chuẩn hoá, lưu vào mcp_server_versions.snapshot
	Hash            []byte          // hash của toàn bộ ảnh chụp
}

// canonicalJSON mã hoá lại JSON với khoá object được sắp xếp, để hai schema tương
// đương về nội dung nhưng khác thứ tự khoá hay khoảng trắng có cùng hash.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// ToolHash là hash ghim định nghĩa của một tool: name + description + inputSchema.
func ToolHash(name, description string, schema json.RawMessage) []byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte(name), []byte(description), schema} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return h.Sum(nil)
}

// normalize kiểm tra và chuẩn hoá danh sách tool của server. Server có tool sai
// dạng bị từ chối toàn bộ: không đăng ký một phần.
func normalize(tools []*mcp.Tool) ([]DiscoveredTool, error) {
	if len(tools) > maxTools {
		return nil, fmt.Errorf("server khai %d tool, tối đa %d", len(tools), maxTools)
	}
	seen := map[string]bool{}
	out := make([]DiscoveredTool, 0, len(tools))
	for _, t := range tools {
		if t == nil || !toolNameRe.MatchString(t.Name) {
			return nil, errors.New("tên tool không hợp lệ: chỉ nhận chữ, số, _ và -, tối đa 64 ký tự")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("tool %q bị khai hai lần", t.Name)
		}
		seen[t.Name] = true
		if len(t.Description) > maxDescription {
			return nil, fmt.Errorf("mô tả của tool %q quá dài", t.Name)
		}
		// Mô tả đi vào prompt và vào database: phải là văn bản hợp lệ, không có NUL.
		if !utf8.ValidString(t.Description) || strings.ContainsRune(t.Description, 0) {
			return nil, fmt.Errorf("mô tả của tool %q chứa ký tự không hợp lệ", t.Name)
		}
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("input schema của tool %q không hợp lệ", t.Name)
		}
		schema, err := canonicalJSON(raw)
		var obj map[string]any
		if err != nil || len(schema) > maxSchemaBytes || json.Unmarshal(schema, &obj) != nil || obj["type"] != "object" {
			return nil, fmt.Errorf("input schema của tool %q phải là một JSON Schema kiểu object, tối đa 64 KiB", t.Name)
		}
		d := DiscoveredTool{Name: t.Name, Description: t.Description, InputSchema: schema}
		if t.Annotations != nil {
			d.ReadOnlyHint = t.Annotations.ReadOnlyHint
		}
		d.Hash = ToolHash(d.Name, d.Description, d.InputSchema)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func snapshotOf(tools []DiscoveredTool, protocol, reported string) Snapshot {
	raw, _ := json.Marshal(map[string]any{"tools": tools})
	h := sha256.New()
	for _, t := range tools {
		h.Write(t.Hash)
	}
	return Snapshot{ProtocolVersion: protocol, ReportedVersion: reported, Tools: tools, Raw: raw, Hash: h.Sum(nil)}
}

// Discover kết nối tới một MCP server qua Streamable HTTP và đọc danh sách tool.
// hc mang cơ chế xác thực với server (ví dụ signedctx.Transport cho managed server).
// endpoint do platform admin cung cấp; URL do tenant cung cấp phải qua SSRF guard
// trước khi tới đây.
func Discover(ctx context.Context, endpoint string, hc *http.Client) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "hq-registry", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("không kết nối được MCP server: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var all []*mcp.Tool
	cursor := ""
	for page := 0; page < 20; page++ {
		res, err := sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return Snapshot{}, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || len(all) > maxTools {
			break
		}
		cursor = res.NextCursor
	}
	tools, err := normalize(all)
	if err != nil {
		return Snapshot{}, err
	}
	protocol, reported := "", ""
	if init := sess.InitializeResult(); init != nil {
		protocol = init.ProtocolVersion
		if init.ServerInfo != nil {
			reported = init.ServerInfo.Version
		}
	}
	if protocol == "" {
		protocol = "unknown"
	}
	return snapshotOf(tools, protocol, reported), nil
}

// WireSnapshot là dạng truyền của một ảnh chụp discovery giữa Gateway (nơi gọi MCP
// server) và API (nơi ghi Registry). Hash không đi trên dây: bên nhận tự tính lại.
type WireSnapshot struct {
	ProtocolVersion string           `json:"protocol_version"`
	ReportedVersion string           `json:"reported_version"`
	Tools           []DiscoveredTool `json:"tools"`
}

// Wire trả về dạng truyền của ảnh chụp.
func (s Snapshot) Wire() WireSnapshot {
	return WireSnapshot{ProtocolVersion: s.ProtocolVersion, ReportedVersion: s.ReportedVersion, Tools: s.Tools}
}

// FromWire dựng lại Snapshot từ dạng truyền, kiểm tra và chuẩn hoá lại từng tool như
// khi đọc thẳng từ server: bên nhận không tin dữ liệu đã được bên gửi kiểm.
func FromWire(w WireSnapshot) (Snapshot, error) {
	if len(w.Tools) > maxTools {
		return Snapshot{}, fmt.Errorf("server khai quá %d tool", maxTools)
	}
	if len(w.ProtocolVersion) > 64 || len(w.ReportedVersion) > 128 {
		return Snapshot{}, errors.New("phiên bản do server khai quá dài")
	}
	w.ProtocolVersion, w.ReportedVersion = printable(w.ProtocolVersion), printable(w.ReportedVersion)
	raw := make([]*mcp.Tool, len(w.Tools))
	for i, t := range w.Tools {
		var schema any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return Snapshot{}, fmt.Errorf("tool %q: inputSchema không phải JSON", t.Name)
		}
		raw[i] = &mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: t.ReadOnlyHint}}
	}
	tools, err := normalize(raw)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotOf(tools, w.ProtocolVersion, w.ReportedVersion), nil
}

// printable bỏ ký tự điều khiển khỏi một chuỗi ngắn do server khai (phiên bản).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}
