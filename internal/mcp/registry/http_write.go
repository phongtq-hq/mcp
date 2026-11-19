package registry

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/api"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Permission của phần ghi. Giá trị phải có trong danh mục của authz.
const (
	permServerCreate   = "mcp_server:create"
	permServerUpdate   = "mcp_server:update"
	permServerDelete   = "mcp_server:delete"
	permServerDiscover = "mcp_server:discover"
	permServerReview   = "mcp_server:review"
	permToolUpdate     = "mcp_tool:update"
)

// RegisterWriteRoutes gắn các endpoint để tenant tự quản lý MCP server ngoài: đăng ký,
// sửa, lưu trữ, discovery, duyệt version, phân loại tool. Managed server vẫn chỉ đi
// qua công cụ quản trị (hqadmin).
func RegisterWriteRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("POST /v1/mcp-servers", idem(func(r *http.Request) error {
		return h.authz.Require(r.Context(), permServerCreate, nil)
	}, wrap(h.register)))
	mux.HandleFunc("PATCH /v1/mcp-servers/{serverId}", wrap(h.update))
	mux.HandleFunc("DELETE /v1/mcp-servers/{serverId}", wrap(h.archive))
	mux.HandleFunc("POST /v1/mcp-servers/{serverId}/discover", wrap(h.discover))
	mux.HandleFunc("GET /v1/mcp-servers/{serverId}/versions", wrap(h.versions))
	mux.HandleFunc("POST /v1/mcp-servers/{serverId}/versions/{version}/review", wrap(h.review))
	mux.HandleFunc("GET /v1/mcp-tools", wrap(h.allTools))
	mux.HandleFunc("GET /v1/mcp-tools/{toolId}", wrap(h.getTool))
	mux.HandleFunc("PATCH /v1/mcp-tools/{toolId}", wrap(h.patchTool))
}

// token lấy bearer token từ trường credential. Chỉ nhận đúng một khoá "token".
func token(c *map[string]interface{}) (*string, error) {
	if c == nil {
		return nil, nil
	}
	v, ok := (*c)["token"].(string)
	if !ok || len(*c) != 1 {
		return nil, invalid(`credential phải có dạng {"token": "<bearer token>"}`)
	}
	return &v, nil
}

func (h *handlers) register(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permServerCreate, nil); err != nil {
		return err
	}
	var body api.McpServerCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Transport != "streamable_http" {
		return invalid("transport: tenant chỉ đăng ký được server streamable_http")
	}
	if body.Endpoint == nil {
		return invalid("endpoint là bắt buộc với transport streamable_http")
	}
	tok, err := token(body.Credential)
	if err != nil {
		return err
	}
	s, err := h.svc.Register(r.Context(), RegisterExternal{Slug: body.Slug, Name: body.Name, Description: body.Description,
		Endpoint: *body.Endpoint, AuthMethod: string(body.AuthMethod), Token: tok})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(s.UpdatedAt))
	w.Header().Set("Location", "/v1/mcp-servers/"+s.ID.String())
	httpx.WriteJSON(w, http.StatusCreated, toAPIServer(s))
	return nil
}

// loadFor đọc server (404 nếu không tồn tại hoặc không đọc được) rồi kiểm quyền cho hành động.
func (h *handlers) loadFor(r *http.Request, permission string) (Server, error) {
	s, err := h.loadServer(r)
	if err != nil {
		return Server{}, err
	}
	if err := h.authz.Require(r.Context(), permission, nil); err != nil {
		return Server{}, err
	}
	return s, nil
}

func (h *handlers) update(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadFor(r, permServerUpdate)
	if err != nil {
		return err
	}
	var body api.McpServerUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	// Đổi endpoint sẽ chuyển mọi tool đã duyệt sang một host khác mà không qua duyệt:
	// không cho sửa tại chỗ. Đăng ký server mới cho endpoint mới.
	if body.Endpoint != nil {
		return invalid("endpoint không sửa được; đăng ký server mới cho endpoint mới")
	}
	tok, err := token(body.Credential)
	if err != nil {
		return err
	}
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	u := ServerUpdate{Name: body.Name, Description: body.Description, Token: tok, IfUpdatedAt: ifMatch}
	if body.Status != nil {
		st := string(*body.Status)
		u.Status = &st
	}
	if s, err = h.svc.Update(r.Context(), s.ID, u); err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(s.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIServer(s))
	return nil
}

func (h *handlers) archive(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadFor(r, permServerDelete)
	if err != nil {
		return err
	}
	if err := h.svc.Archive(r.Context(), s.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handlers) discover(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadFor(r, permServerDiscover)
	if err != nil {
		return err
	}
	res, err := h.svc.DiscoverNow(r.Context(), s.ID)
	if err != nil {
		return err
	}
	if s, err = h.svc.GetServer(r.Context(), s.ID); err != nil {
		return err
	}
	out := map[string]any{"server": toAPIServer(s), "version": res.Version, "changed": res.Changed,
		"pending_review": res.Pending, "locked_tools": res.LockedTools}
	if res.LockedTools == nil {
		out["locked_tools"] = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *handlers) versions(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadServer(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	status := q.Get("review_status")
	if status != "" && status != "pending_review" && status != "approved" && status != "rejected" {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "review_status không hợp lệ")
	}
	before := 1 << 30
	if c := q.Get("cursor"); c != "" {
		if before, err = strconv.Atoi(c); err != nil || before < 1 || before > 1<<30 {
			return pagination.ErrInvalidCursor
		}
	}
	vs, next, err := h.svc.Versions(r.Context(), s.ID, status, limit, before)
	if err != nil {
		return err
	}
	data := make([]api.McpServerVersion, len(vs))
	for i, v := range vs {
		count := v.ToolCount
		data[i] = api.McpServerVersion{Id: v.ID, ServerId: v.ServerID, Version: v.Version, ReportedVersion: v.ReportedVersion,
			ProtocolVersion: v.ProtocolVersion, ReviewStatus: api.ReviewStatus(v.ReviewStatus), ReviewedBy: v.ReviewedBy,
			ReviewedAt: v.ReviewedAt, DiscoveredAt: v.DiscoveredAt, ToolCount: &count}
	}
	page := map[string]any{"data": data, "next_cursor": nil}
	if next > 0 {
		page["next_cursor"] = strconv.Itoa(next)
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) review(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadFor(r, permServerReview)
	if err != nil {
		return err
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 || version > 1<<30 {
		return errNotFound
	}
	var body api.ReviewMcpServerVersionJSONBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Decision != "approved" && body.Decision != "rejected" {
		return invalid("decision phải là approved hoặc rejected")
	}
	if err := h.svc.ReviewVersion(r.Context(), s.ID, version, body.Decision == "approved"); err != nil {
		return err
	}
	if s, err = h.svc.GetServer(r.Context(), s.ID); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPIServer(s))
	return nil
}

func (h *handlers) allTools(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permServerRead, nil); err != nil {
		return err
	}
	q := r.URL.Query()
	f := ToolFilter{RiskLevel: q.Get("risk_level")}
	if v := q.Get("server_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "server_id phải là UUID")
		}
		f.ServerID = &id
	}
	if v := q.Get("enabled"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "enabled phải là true hoặc false")
		}
		f.Enabled = &b
	}
	tools, err := h.svc.ActiveTools(r.Context(), f)
	if err != nil {
		return err
	}
	data := make([]api.McpTool, len(tools))
	for i, t := range tools {
		data[i] = toAPITool(t)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": nil})
	return nil
}

func (h *handlers) loadTool(r *http.Request) (Tool, error) {
	id, err := uuid.Parse(r.PathValue("toolId"))
	if err != nil {
		return Tool{}, errNotFound
	}
	t, err := h.svc.GetTool(r.Context(), id)
	if err != nil {
		return Tool{}, err
	}
	if err := h.authz.Require(r.Context(), permServerRead, nil); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return Tool{}, errNotFound
		}
		return Tool{}, err
	}
	return t, nil
}

func (h *handlers) getTool(w http.ResponseWriter, r *http.Request) error {
	t, err := h.loadTool(r)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPITool(t))
	return nil
}

func (h *handlers) patchTool(w http.ResponseWriter, r *http.Request) error {
	t, err := h.loadTool(r)
	if err != nil {
		return err
	}
	if err := h.authz.Require(r.Context(), permToolUpdate, nil); err != nil {
		return err
	}
	var body api.McpToolPolicyUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Cacheable != nil || body.TimeoutMs != nil || body.MaxResultBytes != nil || body.UnitCost != nil {
		return invalid("cacheable, timeout_ms, max_result_bytes, unit_cost chưa sửa được ở phase này")
	}
	c := Classification{Enabled: body.Enabled, Egress: body.Egress}
	if body.RiskLevel != nil {
		v := string(*body.RiskLevel)
		c.RiskLevel = &v
	}
	if body.SideEffect != nil {
		v := string(*body.SideEffect)
		c.SideEffect = &v
	}
	if body.DataSensitivity != nil {
		v := string(*body.DataSensitivity)
		c.DataSensitivity = &v
	}
	if t, err = h.svc.ClassifyTool(r.Context(), t.ID, c); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPITool(t))
	return nil
}
