package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/api"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

const permServerRead = "mcp_server:read"

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint đọc của Registry (tiền tố /v1). Đăng ký, discovery,
// duyệt và phân loại hiện chỉ đi qua công cụ quản trị (hqadmin).
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/mcp-servers", wrap(h.listServers))
	mux.HandleFunc("GET /v1/mcp-servers/{serverId}", wrap(h.getServer))
	mux.HandleFunc("GET /v1/mcp-servers/{serverId}/tools", wrap(h.listTools))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPIServer(s Server) api.McpServer {
	scope := api.McpServerScope(s.Scope)
	auth := api.McpServerAuthMethod(s.AuthMethod)
	out := api.McpServer{
		Id: s.ID, Slug: s.Slug, Name: s.Name, Description: s.Description, Kind: api.McpServerKind(s.Kind),
		Transport: api.McpServerTransport(s.Transport), Status: api.McpServerStatus(s.Status),
		HealthStatus: api.HealthStatus(s.HealthStatus), HealthCheckedAt: s.HealthCheckedAt,
		ActiveVersion: s.ActiveVersion, PendingReviewVersion: s.PendingVersion, Scope: &scope, AuthMethod: &auth,
		CreatedAt: &s.CreatedAt, UpdatedAt: &s.UpdatedAt,
	}
	// Địa chỉ nội bộ của server thuộc platform không trả cho tenant.
	if s.Scope != ScopePlatform {
		out.Endpoint = s.Endpoint
	}
	return out
}

func toAPITool(t Tool) api.McpTool {
	schema := map[string]any{}
	_ = json.Unmarshal(t.InputSchema, &schema)
	sens := api.McpToolDataSensitivity(t.DataSensitivity)
	timeout := int(t.Timeout.Milliseconds())
	desc := t.Description
	return api.McpTool{
		Id: t.ID, ServerId: t.ServerID, Name: t.Name, Description: &desc, InputSchema: schema, Enabled: t.Enabled,
		RiskLevel: api.RiskLevel(t.RiskLevel), SideEffect: api.McpToolSideEffect(t.SideEffect),
		DataSensitivity: &sens, Egress: &t.Egress, Cacheable: &t.Cacheable, TimeoutMs: &timeout, MaxResultBytes: &t.MaxResultBytes,
	}
}

func (h *handlers) listServers(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permServerRead, nil); err != nil {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	servers, next, err := h.svc.ListServers(r.Context(), limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.McpServer `json:"data"`
		NextCursor *string         `json:"next_cursor"`
	}{Data: make([]api.McpServer, len(servers))}
	for i, s := range servers {
		page.Data[i] = toAPIServer(s)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) loadServer(r *http.Request) (Server, error) {
	id, err := uuid.Parse(r.PathValue("serverId"))
	if err != nil {
		return Server{}, errNotFound
	}
	s, err := h.svc.GetServer(r.Context(), id)
	if err != nil {
		return Server{}, err
	}
	if err := h.authz.Require(r.Context(), permServerRead, nil); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return Server{}, errNotFound
		}
		return Server{}, err
	}
	return s, nil
}

func (h *handlers) getServer(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadServer(r)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(s.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIServer(s))
	return nil
}

func (h *handlers) listTools(w http.ResponseWriter, r *http.Request) error {
	s, err := h.loadServer(r)
	if err != nil {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	f := ToolFilter{ServerID: &s.ID, RiskLevel: q.Get("risk_level")}
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
	// Một server có tối đa 200 tool nên trả trọn một trang.
	page := struct {
		Data       []api.McpTool `json:"data"`
		NextCursor *string       `json:"next_cursor"`
	}{Data: make([]api.McpTool, len(tools))}
	for i, t := range tools {
		page.Data[i] = toAPITool(t)
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}
