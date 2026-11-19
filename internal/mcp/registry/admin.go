package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/mcp/registry/store"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

// Các thao tác dưới đây thay đổi Registry và chạy trong transaction do người gọi
// mở. Hiện chúng chỉ được gọi từ đường quản trị (internal/adminops, role app_admin):
// tenant chưa tự đăng ký server được, vì server ngoài cần SSRF guard và Secrets.

// Loại server.
const (
	KindManaged  = "managed"  // do platform vận hành (internal/integration/*)
	KindExternal = "external" // bên thứ ba hoặc tenant
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// RegisterInput mô tả một managed server cần đăng ký.
type RegisterInput struct {
	OrgID       *uuid.UUID // nil = server của platform, mọi org thấy
	Slug        string
	Name        string
	Description *string
	Endpoint    string
}

// RegisterManaged thêm một managed server ở trạng thái registered. Server chưa
// phục vụ tool nào cho tới khi có một version được duyệt (ApplyDiscovery).
func RegisterManaged(ctx context.Context, tx pgxv5.Tx, in RegisterInput) (uuid.UUID, error) {
	if !slugRe.MatchString(in.Slug) || in.Name == "" {
		return uuid.Nil, errors.New("registry: slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$ và name không rỗng")
	}
	u, err := url.Parse(in.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return uuid.Nil, errors.New("registry: endpoint phải là URL http(s), không kèm thông tin đăng nhập")
	}
	// Chỉ đầy đủ khi chạy dưới role quản trị (thấy mọi org); đó là đường duy nhất tới đây.
	taken, err := store.New(tx).SlugUsedInOtherScope(ctx, store.SlugUsedInOtherScopeParams{Slug: in.Slug, Platform: in.OrgID == nil})
	if err != nil {
		return uuid.Nil, err
	}
	if taken {
		return uuid.Nil, fmt.Errorf("registry: slug %q đã được dùng ở phạm vi khác (platform và org dùng chung không gian tên)", in.Slug)
	}
	id, err := store.New(tx).InsertServer(ctx, store.InsertServerParams{
		ID: ids.New(), OrgID: in.OrgID, Slug: in.Slug, Name: in.Name, Description: in.Description,
		Kind: KindManaged, Endpoint: &in.Endpoint, AuthMethod: "service_token",
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return uuid.Nil, fmt.Errorf("registry: slug %q đã được dùng trong phạm vi này", in.Slug)
	}
	return id, err
}

// ApplyResult là kết quả của việc áp một lần discovery vào Registry.
type ApplyResult struct {
	ServerID uuid.UUID
	Version  int
	// Changed = false: định nghĩa tool không đổi so với version đang hoạt động
	// (hoặc đã có một version chờ duyệt y hệt); không có gì được ghi.
	Changed bool
	// Pending = true: version mới chờ duyệt; version đang hoạt động vẫn phục vụ.
	Pending bool
	// LockedTools: tool của version đang hoạt động bị tạm khoá vì định nghĩa của
	// chúng đã đổi hoặc biến mất ở phía server.
	LockedTools []string
}

type policy struct {
	enabled                       bool
	risk, sideEffect, sensitivity string
	egress, cacheable             bool
	timeoutMs, maxResultBytes     int32
}

// Mặc định an toàn nhất: tool mới phát hiện không dùng được cho tới khi được phân loại.
var safestPolicy = policy{risk: "destructive", sideEffect: "non_idempotent", sensitivity: "internal", timeoutMs: 30000, maxResultBytes: 65536}

// ApplyDiscovery so ảnh chụp mới với version đang hoạt động của server.
//
// Không đổi: không ghi gì. Có đổi: tạo version mới. Với autoApprove (lần đăng ký
// đầu của managed server, hoặc pipeline CI của platform) version mới hoạt động
// ngay. Ngược lại version mới ở trạng thái pending_review, version cũ vẫn phục vụ,
// và những tool có định nghĩa đã đổi bị tạm khoá cho tới khi có người duyệt.
func ApplyDiscovery(ctx context.Context, tx pgxv5.Tx, orgID *uuid.UUID, slug string, snap Snapshot, autoApprove bool) (ApplyResult, error) {
	q := store.New(tx)
	srv, err := q.LockServerBySlug(ctx, store.LockServerBySlugParams{Slug: slug, OrgID: orgID})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return ApplyResult{}, fmt.Errorf("registry: không có server %q trong phạm vi này", slug)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{ServerID: srv.ID}

	prev := map[string]store.ListToolsOfVersionRow{}
	if srv.ActiveVersionID != nil {
		active, err := q.GetVersionHash(ctx, store.GetVersionHashParams{ServerID: srv.ID, ID: *srv.ActiveVersionID})
		if err != nil {
			return ApplyResult{}, err
		}
		if bytes.Equal(active.SnapshotHash, snap.Hash) {
			res.Version = int(active.Version)
			return res, nil
		}
		rows, err := q.ListToolsOfVersion(ctx, *srv.ActiveVersionID)
		if err != nil {
			return ApplyResult{}, err
		}
		for _, r := range rows {
			prev[r.Name] = r
		}
	}
	next := int32(1)
	latest, err := q.LatestVersion(ctx, srv.ID)
	switch {
	case err == nil:
		if latest.ReviewStatus == "pending_review" && bytes.Equal(latest.SnapshotHash, snap.Hash) {
			res.Version, res.Pending = int(latest.Version), true
			return res, nil
		}
		next = latest.Version + 1
	case !errors.Is(err, pgxv5.ErrNoRows):
		return ApplyResult{}, err
	}

	status := "pending_review"
	if autoApprove {
		status = "approved"
	}
	var reported *string
	if snap.ReportedVersion != "" {
		reported = &snap.ReportedVersion
	}
	versionID, err := q.InsertVersion(ctx, store.InsertVersionParams{
		ID: ids.New(), OrgID: srv.OrgID, ServerID: srv.ID, Version: next, ReportedVersion: reported,
		ProtocolVersion: snap.ProtocolVersion, Snapshot: snap.Raw, SnapshotHash: snap.Hash, ReviewStatus: status,
	})
	if err != nil {
		return ApplyResult{}, err
	}

	current := map[string][]byte{}
	for _, t := range snap.Tools {
		current[t.Name] = t.Hash
		p := safestPolicy
		if old, ok := prev[t.Name]; ok && bytes.Equal(old.DefinitionHash, t.Hash) {
			// Định nghĩa không đổi: giữ nguyên phân loại đã có.
			p = policy{enabled: old.Enabled, risk: old.RiskLevel, sideEffect: old.SideEffect, sensitivity: old.DataSensitivity,
				egress: old.Egress, cacheable: old.Cacheable, timeoutMs: old.TimeoutMs, maxResultBytes: old.MaxResultBytes}
		} else if srv.Kind == KindManaged && t.ReadOnlyHint {
			// Managed server là code của chính platform, đã qua review và test hợp đồng:
			// tool nó khai là chỉ đọc được phân loại read và bật sẵn. Annotation của
			// server ngoài không bao giờ được tin theo cách này.
			p.enabled, p.risk, p.sideEffect = true, "read", "none"
		}
		desc := t.Description
		if err := q.InsertTool(ctx, store.InsertToolParams{
			ID: ids.New(), OrgID: srv.OrgID, ServerID: srv.ID, ServerVersionID: versionID,
			Name: t.Name, Description: &desc, InputSchema: t.InputSchema, DefinitionHash: t.Hash,
			Enabled: p.enabled, RiskLevel: p.risk, SideEffect: p.sideEffect, DataSensitivity: p.sensitivity,
			Egress: p.egress, Cacheable: p.cacheable, TimeoutMs: p.timeoutMs, MaxResultBytes: p.maxResultBytes,
		}); err != nil {
			return ApplyResult{}, err
		}
	}
	res.Version, res.Changed = int(next), true

	if autoApprove {
		return res, activate(ctx, q, srv.ID, versionID, slug)
	}
	res.Pending = true
	var lock []uuid.UUID
	for name, old := range prev {
		if h, ok := current[name]; !ok || !bytes.Equal(h, old.DefinitionHash) {
			if old.Enabled {
				lock = append(lock, old.ID)
				res.LockedTools = append(res.LockedTools, name)
			}
		}
	}
	sort.Strings(res.LockedTools)
	if len(lock) > 0 {
		if err := q.DisableToolsByID(ctx, lock); err != nil {
			return ApplyResult{}, err
		}
	}
	return res, nil
}

// Review duyệt hoặc từ chối một version đang chờ. Duyệt thì version đó trở thành
// version đang hoạt động.
func Review(ctx context.Context, tx pgxv5.Tx, orgID *uuid.UUID, slug string, version int, approve bool, reviewer *uuid.UUID) error {
	q := store.New(tx)
	srv, err := q.LockServerBySlug(ctx, store.LockServerBySlugParams{Slug: slug, OrgID: orgID})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return fmt.Errorf("registry: không có server %q trong phạm vi này", slug)
	}
	if err != nil {
		return err
	}
	v, err := q.GetVersionByNumber(ctx, store.GetVersionByNumberParams{ServerID: srv.ID, Version: int32(version)}) //nolint:gosec // số version nhỏ
	if errors.Is(err, pgxv5.ErrNoRows) {
		return fmt.Errorf("registry: server %q không có version %d", slug, version)
	}
	if err != nil {
		return err
	}
	status := "rejected"
	if approve {
		status = "approved"
		// Chỉ duyệt được ảnh chụp mới nhất: duyệt một version cũ sẽ kích hoạt định
		// nghĩa tool mà server không còn phục vụ.
		latest, err := q.LatestVersion(ctx, srv.ID)
		if err != nil {
			return err
		}
		if latest.ID != v.ID {
			return fmt.Errorf("registry: version %d của %q không phải version mới nhất (%d)", version, slug, latest.Version)
		}
	}
	n, err := q.ReviewVersion(ctx, store.ReviewVersionParams{ServerID: srv.ID, ID: v.ID, ReviewStatus: status, ReviewedBy: reviewer})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("registry: version %d của %q không ở trạng thái chờ duyệt", version, slug)
	}
	if approve {
		if srv.ActiveVersionID != nil {
			if err := q.InheritPolicy(ctx, store.InheritPolicyParams{NewVersionID: v.ID, ActiveVersionID: *srv.ActiveVersionID}); err != nil {
				return err
			}
		}
		return activate(ctx, q, srv.ID, v.ID, slug)
	}
	return nil
}

// Classification là thay đổi thuộc tính chính sách của một tool. Trường nil giữ nguyên.
type Classification struct {
	Enabled         *bool
	RiskLevel       *string
	SideEffect      *string
	DataSensitivity *string
	Egress          *bool
}

// Classify đặt thuộc tính chính sách cho một tool của version đang hoạt động.
func Classify(ctx context.Context, tx pgxv5.Tx, orgID *uuid.UUID, slug, tool string, c Classification) error {
	q := store.New(tx)
	srv, err := q.LockServerBySlug(ctx, store.LockServerBySlugParams{Slug: slug, OrgID: orgID})
	if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && srv.ActiveVersionID == nil) {
		return fmt.Errorf("registry: không có server %q đang hoạt động trong phạm vi này", slug)
	}
	if err != nil {
		return err
	}
	// Tool bị tạm khoá vì định nghĩa của nó đã đổi ở phía server (mcp-platform §5.3)
	// không bật lại được bằng phân loại: phải duyệt hoặc từ chối version đang chờ trước.
	if c.Enabled != nil && *c.Enabled {
		latest, err := q.LatestVersion(ctx, srv.ID)
		if err != nil {
			return err
		}
		if latest.ReviewStatus == "pending_review" && latest.ID != *srv.ActiveVersionID {
			hash := func(versionID uuid.UUID) ([]byte, bool, error) {
				tools, err := q.ListToolsOfVersion(ctx, versionID)
				if err != nil {
					return nil, false, err
				}
				for _, t := range tools {
					if t.Name == tool {
						return t.DefinitionHash, true, nil
					}
				}
				return nil, false, nil
			}
			active, ok, err := hash(*srv.ActiveVersionID)
			if err != nil {
				return err
			}
			pending, still, err := hash(latest.ID)
			if err != nil {
				return err
			}
			if ok && (!still || !bytes.Equal(active, pending)) {
				return fmt.Errorf("registry: tool %q đang bị khoá vì định nghĩa của nó đã đổi; duyệt hoặc từ chối version %d trước", tool, latest.Version)
			}
		}
	}
	n, err := q.ClassifyTool(ctx, store.ClassifyToolParams{
		ServerVersionID: *srv.ActiveVersionID, Name: tool, Enabled: c.Enabled, RiskLevel: c.RiskLevel,
		SideEffect: c.SideEffect, DataSensitivity: c.DataSensitivity, Egress: c.Egress,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("registry: server %q không có tool %q", slug, tool)
	}
	return nil
}

func activate(ctx context.Context, q *store.Queries, serverID, versionID uuid.UUID, slug string) error {
	n, err := q.ActivateVersion(ctx, store.ActivateVersionParams{ID: serverID, ActiveVersionID: &versionID})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("registry: server %q đã bị tắt hoặc lưu trữ; không kích hoạt version mới", slug)
	}
	return nil
}
