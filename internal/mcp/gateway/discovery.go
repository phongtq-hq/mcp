package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
)

// Discovery của MCP server ngoài.
//
// Process api không gọi MCP server (overview §4): khi một quản trị viên yêu cầu
// discovery, api uỷ việc gọi server cho Gateway bằng một service token ngắn hạn chỉ
// dùng được cho đúng server đó. Gateway gọi server qua SSRF guard, kèm credential
// của server, và trả ảnh chụp về; việc ghi Registry và quyết định duyệt thuộc về api.

// PurposeDiscovery là mục đích của service token dùng cho discovery.
const PurposeDiscovery = "mcp_discovery"

// RegisterRegistryRoutes gắn endpoint discovery nội bộ.
func (g *Gateway) RegisterRegistryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /registry/internal/v1/servers/{server}/discover", g.serveDiscovery)
}

func (g *Gateway) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	if g.svcTokens == nil || g.ext == nil {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := g.svcTokens.VerifyService(token, PurposeDiscovery, time.Now())
	if !ok || err != nil {
		httpx.WriteError(w, r, errUnauthenticated)
		return
	}
	serverID, err := uuid.Parse(r.PathValue("server"))
	if err != nil || serverID != claims.Resource {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	p := identity.Principal{Type: claims.ActorType, ID: claims.ActorID, OrgID: claims.OrgID}
	ctx := tenancy.WithOrg(identity.WithPrincipal(r.Context(), p), claims.OrgID)
	ctx = audit.WithActor(ctx, audit.Actor{Type: p.Type, ID: &p.ID})
	// Hạn mức discovery tính theo org, không theo actor trong token.
	if !g.limit(w, r, identity.Principal{Type: p.Type, OrgID: p.OrgID}, classDiscovery) {
		return
	}
	srv, err := g.ext.Servers.ExternalEndpoint(ctx, serverID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	hc, err := g.externalClient(ctx, srv.AuthMethod, srv.SecretID, PurposeDiscovery)
	if err != nil {
		slog.ErrorContext(ctx, "gateway: không dựng được client cho discovery", "error", err, "server_id", serverID)
		httpx.WriteError(w, r, apperr.New(apperr.KindInternal, "internal_error", ""))
		return
	}
	snap, err := registry.Discover(ctx, srv.Endpoint, hc)
	outcome := audit.OutcomeSuccess
	if err != nil {
		outcome = audit.OutcomeError
	}
	// Mỗi lần Gateway gọi ra một server ngoài đều để lại dấu vết, kể cả khi thất bại.
	actx, cancel := contextForAudit(ctx, p.OrgID)
	aerr := tenancy.InTx(actx, g.db, func(tx pgxv5.Tx) error {
		return audit.Record(actx, tx, audit.Event{OrgID: p.OrgID, Action: "mcp_server.discovery_call", ResourceType: "mcp_server",
			ResourceID: &serverID, Outcome: outcome, Details: map[string]string{"slug": srv.Slug}})
	})
	cancel()
	if aerr != nil {
		slog.ErrorContext(ctx, "gateway: không ghi được audit cho discovery", "error", aerr)
		httpx.WriteError(w, r, apperr.New(apperr.KindInternal, "internal_error", ""))
		return
	}
	if err != nil {
		// Chi tiết (địa chỉ, lỗi kết nối) chỉ vào log: phản hồi không là công cụ dò mạng.
		slog.WarnContext(ctx, "gateway: discovery lỗi", "error", err, "server_id", serverID)
		if errors.Is(err, safehttp.ErrBlocked) {
			httpx.WriteError(w, r, apperr.New(apperr.KindUnprocessable, "endpoint_blocked", "endpoint của server không được phép gọi"))
			return
		}
		httpx.WriteError(w, r, apperr.New(apperr.KindUnprocessable, "discovery_failed",
			"không đọc được danh sách tool từ server: không kết nối được, bị từ chối, hoặc định nghĩa tool không hợp lệ"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, snap.Wire())
}
