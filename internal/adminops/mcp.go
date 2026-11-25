package adminops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// MCPServer xác định một server trong Registry. OrgSlug rỗng = server của platform
// (mọi org thấy); có giá trị = server của riêng org đó.
type MCPServer struct {
	OrgSlug string
	Slug    string
}

// adminTx chạy fn dưới role app_admin. Nếu server thuộc một org, transaction mang
// tenant đó để ghi audit vào chuỗi của org.
func adminTx(ctx context.Context, conn *pgxv5.Conn, orgSlug string, fn func(tx pgxv5.Tx, orgID *uuid.UUID) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_admin"); err != nil {
		return fmt.Errorf("tài khoản kết nối phải là thành viên của app_admin: %w", err)
	}
	var orgID *uuid.UUID
	if orgSlug != "" {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, orgSlug).Scan(&id); err != nil {
			if errors.Is(err, pgxv5.ErrNoRows) {
				return fmt.Errorf("không có organization %q", orgSlug)
			}
			return err
		}
		orgID = &id
		if _, err := tx.Exec(ctx, "SELECT set_config('app.org_id', $1, true)", id.String()); err != nil {
			return err
		}
	}
	if err := fn(tx, orgID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recordMCP(ctx context.Context, tx pgxv5.Tx, orgID *uuid.UUID, action string, serverID uuid.UUID, details map[string]string) error {
	if orgID == nil {
		// audit_logs chưa có chuỗi cho sự kiện cấp platform (còn nợ). Tạm thời ghi log
		// có cấu trúc để thay đổi tác động tới mọi tenant vẫn để lại dấu vết.
		slog.WarnContext(ctx, "platform registry change (chưa có audit cấp platform)",
			"action", action, "mcp_server_id", serverID, "details", details)
		return nil
	}
	return audit.Record(ctx, tx, audit.Event{
		OrgID: *orgID, Action: action, ResourceType: "mcp_server", ResourceID: &serverID,
		Outcome: audit.OutcomeSuccess, Details: details})
}

func discover(ctx context.Context, slug, endpoint string, signer *signedctx.Signer) (registry.Snapshot, error) {
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect, Transport: &signedctx.Transport{
		Signer: signer, Context: signedctx.Context{Kind: signedctx.KindDiscovery, Audience: slug}}}
	return registry.Discover(ctx, endpoint, hc)
}

// RegisterManagedMCPServer đăng ký một managed MCP server, chạy discovery lần đầu và
// duyệt luôn version đầu tiên: managed server là code của chính platform, người vận
// hành đăng ký nó là người chịu trách nhiệm về định nghĩa tool của nó.
func RegisterManagedMCPServer(ctx context.Context, conn *pgxv5.Conn, signer *signedctx.Signer, s MCPServer, name, endpoint string) (registry.ApplyResult, error) {
	snap, err := discover(ctx, s.Slug, endpoint, signer)
	if err != nil {
		return registry.ApplyResult{}, err
	}
	var res registry.ApplyResult
	err = adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		if _, err := registry.RegisterManaged(ctx, tx, registry.RegisterInput{OrgID: orgID, Slug: s.Slug, Name: name, Endpoint: endpoint}); err != nil {
			return err
		}
		var err error
		res, err = registry.ApplyDiscovery(ctx, tx, orgID, s.Slug, snap, true)
		if err != nil {
			return err
		}
		return recordMCP(ctx, tx, orgID, "mcp_server.register", res.ServerID,
			map[string]string{"slug": s.Slug, "version": fmt.Sprint(res.Version), "tools": fmt.Sprint(len(snap.Tools))})
	})
	return res, err
}

// SetManagedMCPServerEndpoint đổi địa chỉ của một managed MCP server đã đăng ký (server
// chuyển máy, hoặc chuyển từ chạy trực tiếp sang chạy trong container).
//
// Discovery ở địa chỉ mới KHÔNG chứng minh đó là đúng server: chữ ký ngữ cảnh chỉ xác
// thực request của platform, và bất kỳ host nào trả về danh sách tool giống hệt đều qua.
// Người vận hành chạy lệnh này chịu trách nhiệm về địa chỉ, như khi đăng ký. Những gì
// hàm này bảo đảm:
//   - chỉ managed server của một organization (server phạm vi platform bị từ chối cho
//     tới khi có audit cấp platform: đổi địa chỉ của nó tác động mọi tenant mà không để
//     lại bản ghi nào);
//   - https, trừ khi allowHTTP (người gọi chỉ đặt nó ở môi trường local): ngữ cảnh ký và
//     arguments của tool đi trên đường nối này;
//   - định nghĩa tool ở địa chỉ mới đi qua luật của discovery (khác đi → version chờ
//     duyệt, tool bị khoá), nên đổi địa chỉ không phải đường tắt để thay tool;
//   - audit `mcp_server.set_endpoint` kèm địa chỉ cũ và mới, cùng transaction.
func SetManagedMCPServerEndpoint(ctx context.Context, conn *pgxv5.Conn, signer *signedctx.Signer, s MCPServer, endpoint string, allowHTTP bool) (registry.ApplyResult, error) {
	if s.OrgSlug == "" {
		return registry.ApplyResult{}, errors.New("set-endpoint chỉ áp cho managed server của một organization (-org); server phạm vi platform chưa đổi địa chỉ được vì chưa có audit cấp platform")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return registry.ApplyResult{}, errors.New("endpoint phải là URL http(s), không kèm thông tin đăng nhập, query hay fragment")
	}
	if u.Scheme != "https" && !allowHTTP {
		return registry.ApplyResult{}, errors.New("endpoint phải là https (http chỉ được nhận khi HQ_ENV được đặt tường minh là local)")
	}
	snap, err := discover(ctx, s.Slug, endpoint, signer)
	if err != nil {
		return registry.ApplyResult{}, err
	}
	var res registry.ApplyResult
	err = adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		var id uuid.UUID
		var old string
		// Khoá dòng rồi mới đọc địa chỉ cũ, để audit ghi đúng cặp cũ → mới.
		err := tx.QueryRow(ctx, `SELECT id, endpoint FROM mcp_servers
			WHERE slug = $1 AND org_id IS NOT DISTINCT FROM $2 AND kind = 'managed' AND endpoint IS NOT NULL FOR UPDATE`, s.Slug, orgID).Scan(&id, &old)
		if errors.Is(err, pgxv5.ErrNoRows) {
			return fmt.Errorf("không có managed server %q trong phạm vi này", s.Slug)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE mcp_servers SET endpoint = $2 WHERE id = $1`, id, endpoint); err != nil {
			return err
		}
		if res, err = registry.ApplyDiscovery(ctx, tx, orgID, s.Slug, snap, false); err != nil {
			return err
		}
		return recordMCP(ctx, tx, orgID, "mcp_server.set_endpoint", id,
			map[string]string{"slug": s.Slug, "old_endpoint": old, "new_endpoint": endpoint, "definition_changed": fmt.Sprint(res.Changed)})
	})
	return res, err
}

// DiscoverMCPServer chạy lại discovery. Định nghĩa tool không đổi thì không ghi gì;
// có đổi thì tạo version chờ duyệt và tạm khoá các tool bị đổi.
func DiscoverMCPServer(ctx context.Context, conn *pgxv5.Conn, signer *signedctx.Signer, s MCPServer) (registry.ApplyResult, error) {
	var endpoint string
	if err := adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		return tx.QueryRow(ctx, `SELECT endpoint FROM mcp_servers WHERE slug = $1 AND org_id IS NOT DISTINCT FROM $2 AND endpoint IS NOT NULL`,
			s.Slug, orgID).Scan(&endpoint)
	}); err != nil {
		if errors.Is(err, pgxv5.ErrNoRows) {
			return registry.ApplyResult{}, fmt.Errorf("không có server %q trong phạm vi này", s.Slug)
		}
		return registry.ApplyResult{}, err
	}
	snap, err := discover(ctx, s.Slug, endpoint, signer)
	if err != nil {
		return registry.ApplyResult{}, err
	}
	var res registry.ApplyResult
	err = adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		var err error
		res, err = registry.ApplyDiscovery(ctx, tx, orgID, s.Slug, snap, false)
		if err != nil || !res.Changed {
			return err
		}
		return recordMCP(ctx, tx, orgID, "mcp_server.definition_changed", res.ServerID,
			map[string]string{"slug": s.Slug, "pending_version": fmt.Sprint(res.Version), "locked_tools": fmt.Sprint(res.LockedTools),
				"kept_disabled": fmt.Sprint(res.KeptDisabled)})
	})
	return res, err
}

// ReviewMCPServerVersion duyệt hoặc từ chối một version đang chờ.
func ReviewMCPServerVersion(ctx context.Context, conn *pgxv5.Conn, s MCPServer, version int, approve bool) error {
	return adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		if err := registry.Review(ctx, tx, orgID, s.Slug, version, approve, nil); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM mcp_servers WHERE slug = $1 AND org_id IS NOT DISTINCT FROM $2`, s.Slug, orgID).Scan(&id); err != nil {
			return err
		}
		return recordMCP(ctx, tx, orgID, "mcp_server.version.review", id,
			map[string]string{"slug": s.Slug, "version": fmt.Sprint(version), "approved": fmt.Sprint(approve)})
	})
}

// ClassifyMCPTool đặt thuộc tính chính sách cho một tool của version đang hoạt động.
func ClassifyMCPTool(ctx context.Context, conn *pgxv5.Conn, s MCPServer, tool string, c registry.Classification) error {
	return adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		if err := registry.Classify(ctx, tx, orgID, s.Slug, tool, c); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM mcp_servers WHERE slug = $1 AND org_id IS NOT DISTINCT FROM $2`, s.Slug, orgID).Scan(&id); err != nil {
			return err
		}
		d := map[string]string{"slug": s.Slug, "tool": tool}
		if c.Enabled != nil {
			d["enabled"] = fmt.Sprint(*c.Enabled)
		}
		if c.RiskLevel != nil {
			d["risk_level"] = *c.RiskLevel
		}
		if c.DataSensitivity != nil {
			d["data_sensitivity"] = *c.DataSensitivity
		}
		if c.Egress != nil {
			d["egress"] = fmt.Sprint(*c.Egress)
		}
		return recordMCP(ctx, tx, orgID, "mcp_tool.classify", id, d)
	})
}

// noRedirect: ngữ cảnh ký không được đi theo redirect tới một host khác.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ScopeMCPServer đặt chính sách project của một server thuộc org (migration 00041): scoped
// bật / tắt việc giới hạn theo project (nil giữ nguyên), add / remove là slug của project
// cần gắn / gỡ. Trả về trạng thái sau thay đổi. Server của platform không giới hạn theo
// project được. Mọi thay đổi ghi audit mcp_server.project_scope.
func ScopeMCPServer(ctx context.Context, conn *pgxv5.Conn, s MCPServer, scoped *bool, add, remove []string) (bool, []string, error) {
	if s.OrgSlug == "" {
		return false, nil, errors.New("chỉ server của một organization giới hạn theo project được (-org)")
	}
	var nowScoped bool
	var bound []string
	err := adminTx(ctx, conn, s.OrgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM mcp_servers WHERE slug = $1 AND org_id = $2 FOR UPDATE`, s.Slug, *orgID).Scan(&id); err != nil {
			if errors.Is(err, pgxv5.ErrNoRows) {
				return fmt.Errorf("không có server %q trong organization %q", s.Slug, s.OrgSlug)
			}
			return err
		}
		project := func(slug string) (uuid.UUID, error) {
			var pid uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE org_id = $1 AND slug = $2`, *orgID, slug).Scan(&pid)
			if errors.Is(err, pgxv5.ErrNoRows) {
				return uuid.Nil, fmt.Errorf("không có project %q trong organization %q", slug, s.OrgSlug)
			}
			return pid, err
		}
		for _, slug := range add {
			pid, err := project(slug)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO mcp_server_projects (org_id, server_id, project_id) VALUES ($1,$2,$3)
				ON CONFLICT DO NOTHING`, *orgID, id, pid); err != nil {
				return err
			}
		}
		for _, slug := range remove {
			pid, err := project(slug)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM mcp_server_projects WHERE org_id = $1 AND server_id = $2 AND project_id = $3`, *orgID, id, pid); err != nil {
				return err
			}
		}
		if scoped != nil {
			if _, err := tx.Exec(ctx, `UPDATE mcp_servers SET project_scoped = $1, updated_at = clock_timestamp() WHERE id = $2`, *scoped, id); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT project_scoped FROM mcp_servers WHERE id = $1`, id).Scan(&nowScoped); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.slug FROM mcp_server_projects b JOIN projects p ON p.org_id = b.org_id AND p.id = b.project_id
			WHERE b.org_id = $1 AND b.server_id = $2 ORDER BY p.slug`, *orgID, id)
		if err != nil {
			return err
		}
		bound, err = pgxv5.CollectRows(rows, pgxv5.RowTo[string])
		if err != nil {
			return err
		}
		return recordMCP(ctx, tx, orgID, "mcp_server.project_scope", id, map[string]string{
			"slug": s.Slug, "project_scoped": fmt.Sprint(nowScoped), "projects": fmt.Sprint(bound),
			"added": fmt.Sprint(add), "removed": fmt.Sprint(remove)})
	})
	return nowScoped, bound, err
}
