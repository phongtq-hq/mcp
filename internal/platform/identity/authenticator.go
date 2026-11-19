package identity

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity/store"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/ids"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

var (
	errUnauthenticated = apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu hoặc sai thông tin xác thực")
	errUserDisabled    = apperr.New(apperr.KindPermissionDenied, "user_disabled", "tài khoản đã bị vô hiệu hoá")
	errOrgInactive     = apperr.New(apperr.KindPermissionDenied, "organization_inactive", "organization đã bị đình chỉ hoặc lưu trữ")
	errOrgRequired     = apperr.New(apperr.KindPermissionDenied, "organization_required",
		"không xác định được organization cho request: tài khoản không thuộc organization nào, hoặc thuộc nhiều organization mà token không chỉ rõ")
)

// Authenticator xác thực request và xác lập principal + tenant trong context.
// Đây là nơi duy nhất tenant của một request được quyết định.
type Authenticator struct {
	verifier TokenVerifier
	db       *pgxpool.Pool
	// provision: tạo bản ghi user ở lần đăng nhập đầu. Chỉ process api làm việc này.
	provision bool
	// keys xác thực API key của platform; nil thì chỉ nhận token OIDC.
	keys KeyAuthenticator
}

// APIKeyPrefix là dấu nhận biết API key của platform trong header Authorization.
const APIKeyPrefix = "hqk_"

// KeyAuthenticator xác thực một API key của platform.
type KeyAuthenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// WithAPIKeys trả về Authenticator nhận thêm API key của platform.
func (a *Authenticator) WithAPIKeys(k KeyAuthenticator) *Authenticator {
	c := *a
	c.keys = k
	return &c
}

// NewAuthenticator tạo Authenticator.
func NewAuthenticator(verifier TokenVerifier, db *pgxpool.Pool) *Authenticator {
	return &Authenticator{verifier: verifier, db: db, provision: true}
}

// WithoutProvisioning trả về Authenticator không tạo user mới: token của người chưa
// từng đăng nhập vào platform bị từ chối. Dùng cho gateway, vốn chỉ có quyền đọc
// bảng users.
func (a *Authenticator) WithoutProvisioning() *Authenticator {
	c := *a
	c.provision = false
	return &c
}

// Middleware từ chối request chưa xác thực; request hợp lệ đi tiếp với
// identity.Principal và tenant đã gắn vào context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			if apperr.KindOf(err) == apperr.KindUnauthenticated {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hq-api"`)
			}
			httpx.WriteError(w, r, err)
			return
		}
		ctx := WithPrincipal(r.Context(), p)
		ctx = tenancy.WithOrg(ctx, p.OrgID)
		actor := audit.Actor{Type: audit.ActorUser, ID: &p.ID}
		if p.Type == PrincipalAPIKey {
			actor.Type = audit.ActorAPIKey
		}
		// Địa chỉ kết nối trực tiếp. X-Forwarded-For không được tin: client tự đặt được.
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			actor.IP = ap.Addr()
		}
		ctx = audit.WithActor(ctx, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Authenticator) authenticate(r *http.Request) (Principal, error) {
	ctx := r.Context()
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Principal{}, errUnauthenticated
	}
	// API key của platform có dạng riêng và không bao giờ là JWT: nó không đi qua
	// bộ kiểm token OIDC, và token OIDC không đi qua đường này.
	if strings.HasPrefix(token, APIKeyPrefix) {
		if a.keys == nil {
			return Principal{}, errUnauthenticated
		}
		return a.keys.Authenticate(ctx, token)
	}
	claims, err := a.verifier.Verify(ctx, token)
	if err != nil {
		if errors.Is(err, ErrIdPUnavailable) {
			// Lỗi hạ tầng, không phải token sai: 503 để client không bỏ token và đăng nhập lại.
			return Principal{}, apperr.Wrap(err, apperr.KindUnavailable, "idp_unavailable",
				"tạm thời không kiểm tra được thông tin xác thực").AsRetryable()
		}
		// Lý do cụ thể chỉ vào log (mức debug: request chưa xác thực không được
		// làm ngập log); client chỉ biết là không xác thực được.
		slog.DebugContext(ctx, "token rejected", "reason", err.Error())
		return Principal{}, errUnauthenticated
	}

	userID, err := a.resolveUser(ctx, claims)
	if err != nil {
		return Principal{}, err
	}
	orgID, err := a.resolveOrg(ctx, userID, claims.OrgHint)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Type: PrincipalUser, ID: userID, OrgID: orgID}, nil
}

// resolveUser tìm user theo (iss, sub), tạo ở lần đăng nhập đầu tiên.
func (a *Authenticator) resolveUser(ctx context.Context, c Claims) (uuid.UUID, error) {
	q := store.New(a.db)
	key := store.GetUserByIdentityParams{IdpIssuer: c.Issuer, IdpSubject: c.Subject}
	u, err := q.GetUserByIdentity(ctx, key)
	if errors.Is(err, pgxv5.ErrNoRows) && !a.provision {
		return uuid.Nil, errOrgRequired
	}
	if errors.Is(err, pgxv5.ErrNoRows) {
		var name *string
		if c.Name != "" {
			name = &c.Name
		}
		if err := q.InsertUser(ctx, store.InsertUserParams{
			ID: ids.New(), IdpIssuer: c.Issuer, IdpSubject: c.Subject, Email: c.Email, DisplayName: name,
		}); err != nil {
			return uuid.Nil, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
		}
		u, err = q.GetUserByIdentity(ctx, key)
	}
	if err != nil {
		return uuid.Nil, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	if u.Status != "active" {
		return uuid.Nil, errUserDisabled
	}
	return u.ID, nil
}

// resolveOrg chọn tenant cho request từ membership đang hoạt động của user.
// Claim org trong token chỉ là gợi ý: luôn được đối chiếu với membership.
func (a *Authenticator) resolveOrg(ctx context.Context, userID uuid.UUID, hint string) (uuid.UUID, error) {
	var orgs []uuid.UUID
	err := sharedpgx.WithUserTx(ctx, a.db, userID, func(tx pgxv5.Tx) error {
		var err error
		orgs, err = store.New(tx).ListActiveOrgIDsForUser(ctx, userID)
		return err
	})
	if err != nil {
		return uuid.Nil, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	var orgID uuid.UUID
	switch {
	case hint != "":
		want, err := uuid.Parse(hint)
		if err != nil || !slices.Contains(orgs, want) {
			return uuid.Nil, errOrgRequired
		}
		orgID = want
	case len(orgs) == 1:
		orgID = orgs[0]
	default:
		return uuid.Nil, errOrgRequired
	}

	// Organization bị đình chỉ hoặc lưu trữ không phục vụ request nào. Kiểm ở đây,
	// điểm duy nhất tenant được chọn, để mọi endpoint về sau đều được bảo vệ.
	var status string
	err = sharedpgx.WithTenantTx(ctx, a.db, orgID, func(tx pgxv5.Tx) error {
		var err error
		status, err = store.New(tx).GetOrganizationStatus(ctx, orgID)
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return uuid.Nil, errOrgRequired
	}
	if err != nil {
		return uuid.Nil, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	if status != "active" {
		return uuid.Nil, errOrgInactive
	}
	return orgID, nil
}
