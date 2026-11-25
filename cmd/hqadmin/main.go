// Command hqadmin là công cụ quản trị xuyên tenant cho người vận hành.
// Kết nối bằng HQ_ADMIN_DATABASE_URL, tài khoản phải là thành viên của role app_admin.
//
// Dùng:
//
//	hqadmin org create -slug acme -name "Acme" -admin-issuer https://idp/realms/hq -admin-subject <sub> [-admin-email a@b]
//	hqadmin mcp-server register -slug docker -name "Docker" -endpoint http://localhost:8090/mcp [-org acme]
//	hqadmin mcp-server discover -slug docker [-org acme]
//	hqadmin mcp-server review   -slug docker -version 2 -approve|-reject [-org acme]
//	hqadmin mcp-server scope    -slug pricing-engine -org acme [-scoped=true|false] [-add ops,finance] [-remove sandbox]   giới hạn server theo project (migration 00041)
//	hqadmin mcp-tool classify   -server docker -tool X [-enabled=true] [-risk read|write|destructive|financial] [-sensitivity public|internal|sensitive] [-egress=true] [-org acme]
//	hqadmin audit verify        [-org acme] [-allow-truncated-head]   kiểm chuỗi hash của audit (mọi org nếu bỏ -org); đứt, mất đầu, hoặc không kiểm được org nào thì thoát mã khác 0
//	hqadmin audit export        -org acme -from 2026-11-01T00:00:00Z -to 2026-12-01T00:00:00Z -out audit.jsonl
//	hqadmin audit verify-file   -in audit.jsonl [-after prev.jsonl]   kiểm một file đã xuất (và việc nó nối với file của lần xuất trước), không cần database
//	hqadmin temporal-certs init -dir /certs [-server-name temporal]   CA nội bộ + chứng chỉ mTLS cho Temporal tự vận hành (ADR-0017); chạy lại được, không cần database
//	hqadmin service-key init    -out service-signing.key   khoá ký ngữ cảnh Ed25519 (ADR-0018): seed vào file 0600 cho gateway / hqadmin, in khoá công khai cho managed server
//	hqadmin service-key public                             in khoá công khai ứng với HQ_SERVICE_SIGNING_KEY
//
// -org bỏ trống: server của platform, mọi organization thấy. Các lệnh mcp-server
// register / discover cần HQ_SERVICE_SIGNING_KEY để gọi managed server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hqadmin:", err)
		os.Exit(1)
	}
}

const usage = "usage: hqadmin org create | secrets init | service-key init|public | temporal-certs init | maintenance run | audit verify|export|verify-file | mcp-server register|set-endpoint|discover|review|scope | mcp-tool classify  (xem chú thích đầu file hoặc -h của từng lệnh)"

func run(args []string) error {
	if len(args) < 2 {
		return errors.New(usage)
	}
	cmd, rest := args[0]+" "+args[1], args[2:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connect := func() (*pgxv5.Conn, error) {
		dsn := os.Getenv("HQ_ADMIN_DATABASE_URL")
		if dsn == "" {
			return nil, errors.New("HQ_ADMIN_DATABASE_URL is required")
		}
		conn, err := pgxv5.Connect(ctx, dsn)
		if err != nil {
			return nil, errors.New("không kết nối được database") // không in lỗi gốc: có thể chứa DSN
		}
		return conn, nil
	}
	signer := func() (*signedctx.Signer, error) {
		key, err := signedctx.ParseSigningKey(os.Getenv("HQ_SERVICE_SIGNING_KEY"))
		if err != nil {
			return nil, errors.New("HQ_SERVICE_SIGNING_KEY: " + err.Error())
		}
		return signedctx.NewSigner(key)
	}
	var srv adminops.MCPServer
	serverFlags := func(slugFlag string) {
		fs.StringVar(&srv.Slug, slugFlag, "", "slug của MCP server")
		fs.StringVar(&srv.OrgSlug, "org", "", "slug của organization sở hữu server; bỏ trống = server của platform")
	}

	switch cmd {
	case "maintenance run":
		months := fs.Int("months", 3, "số tháng tới cần có sẵn phân vùng")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(ctx) }()
		res, err := adminops.RunMaintenance(ctx, conn, *months, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("phân vùng: %s → %s; đã xoá %d idempotency key hết hạn\n", res.Months[0], res.Months[len(res.Months)-1], res.IdempotencyKeysDeleted)
		if res.AuditRowsInDefault > 0 {
			return fmt.Errorf("có %d dòng audit ở phân vùng mặc định: đã có lúc thiếu phân vùng tháng, cần kiểm tra", res.AuditRowsInDefault)
		}
		return nil
	case "secrets init":
		return secretsInit(args[2:])
	case "service-key init":
		return serviceKeyInit(rest)
	case "service-key public":
		s, err := signer()
		if err != nil {
			return err
		}
		fmt.Printf("HQ_SERVICE_VERIFY_KEY=%s\n", s.PublicKeyHex())
		return nil
	case "temporal-certs init":
		return temporalCertsInit(rest)
	case "audit verify":
		org := fs.String("org", "", "slug của organization; bỏ trống = mọi organization")
		truncated := fs.Bool("allow-truncated-head", false, "chấp nhận chuỗi đã mất phần đầu (chỉ dùng sau khi retention đã xoá phần đó; đối chiếu mỏ neo với file đã xuất)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(ctx) }()
		// Duyệt cả chuỗi: không dùng hạn 60 giây của các lệnh khác (mỗi organization có hạn riêng).
		chains, err := adminops.VerifyAudit(context.Background(), conn, *org, *truncated)
		for _, c := range chains {
			note := ""
			if c.Anchored {
				note = " (kiểm từ bản ghi còn lại đầu tiên; phần đầu của chuỗi không còn)"
			}
			fmt.Printf("%s: %d bản ghi, chuỗi nguyên vẹn%s\n", c.OrgSlug, c.Records, note)
		}
		return err
	case "audit export":
		org := fs.String("org", "", "slug của organization")
		from := fs.String("from", "", "đầu khoảng (RFC 3339, tính cả)")
		to := fs.String("to", "", "cuối khoảng (RFC 3339, không tính)")
		out := fs.String("out", "", "file đích; không ghi đè file đã có")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		start, err1 := time.Parse(time.RFC3339, *from)
		end, err2 := time.Parse(time.RFC3339, *to)
		if err1 != nil || err2 != nil || *out == "" {
			return errors.New("-org, -from, -to (RFC 3339) và -out là bắt buộc")
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(ctx) }()
		// File audit chứa định danh người dùng và tài nguyên: chỉ chủ file đọc được.
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		actx, acancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer acancel()
		m, err := adminops.ExportAudit(actx, conn, *org, start, end, f)
		if err == nil {
			err = f.Sync() // file phải nằm trên đĩa trước khi báo đã xuất
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(*out) // không để lại file dở dang trông như một bản xuất hoàn chỉnh
			return err
		}
		fmt.Printf("đã xuất %d bản ghi vào %s; hash cuối %s\n", m.Records, *out, m.LastHash)
		return nil
	case "audit verify-file":
		in := fs.String("in", "", "file do audit export tạo")
		after := fs.String("after", "", "file của lần xuất ngay trước: kiểm cả việc hai file nối nhau")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		var prev io.Reader
		if *after != "" {
			pf, err := os.Open(*after)
			if err != nil {
				return err
			}
			defer func() { _ = pf.Close() }()
			prev = pf
		}
		m, err := adminops.VerifyAuditFiles(prev, f)
		if err != nil {
			return err
		}
		fmt.Printf("file nguyên vẹn: organization %s, %d bản ghi, %s → %s, hash cuối %s\n", m.OrgID, m.Records,
			m.From.Format(time.RFC3339), m.To.Format(time.RFC3339), m.LastHash)
		return nil
	case "org create":
		var in adminops.BootstrapOrg
		fs.StringVar(&in.Slug, "slug", "", "slug của organization")
		fs.StringVar(&in.Name, "name", "", "tên hiển thị")
		fs.StringVar(&in.AdminIssuer, "admin-issuer", "", "iss của IdP")
		fs.StringVar(&in.AdminSubject, "admin-subject", "", "sub của quản trị viên đầu tiên")
		fs.StringVar(&in.AdminEmail, "admin-email", "", "email của quản trị viên")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		id, err := adminops.CreateOrganization(ctx, conn, in)
		if err != nil {
			return err
		}
		fmt.Println(id)

	case "mcp-server register":
		serverFlags("slug")
		name := fs.String("name", "", "tên hiển thị")
		endpoint := fs.String("endpoint", "", "URL MCP Streamable HTTP của server")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		s, err := signer()
		if err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		res, err := adminops.RegisterManagedMCPServer(ctx, conn, s, srv, *name, *endpoint)
		if err != nil {
			return err
		}
		fmt.Printf("đã đăng ký %s: version %d đang hoạt động\n", srv.Slug, res.Version)

	case "mcp-server set-endpoint":
		serverFlags("slug")
		endpoint := fs.String("endpoint", "", "URL MCP Streamable HTTP mới của server")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		s, err := signer()
		if err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		res, err := adminops.SetManagedMCPServerEndpoint(ctx, conn, s, srv, *endpoint, os.Getenv("HQ_ENV") == "local")
		if err != nil {
			return err
		}
		if res.Changed {
			fmt.Printf("đã đổi địa chỉ của %s; định nghĩa tool ở địa chỉ mới KHÁC: version %d chờ duyệt, tool bị khoá: %s\n", srv.Slug, res.Version, strings.Join(res.LockedTools, ", "))
		} else {
			fmt.Printf("đã đổi địa chỉ của %s; định nghĩa tool không đổi\n", srv.Slug)
		}

	case "mcp-server discover":
		serverFlags("slug")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		s, err := signer()
		if err != nil {
			return err
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		res, err := adminops.DiscoverMCPServer(ctx, conn, s, srv)
		if err != nil {
			return err
		}
		switch {
		case !res.Changed && !res.Pending:
			fmt.Printf("%s: định nghĩa tool không đổi (version %d)\n", srv.Slug, res.Version)
		case !res.Changed:
			fmt.Printf("%s: version %d vẫn đang chờ duyệt\n", srv.Slug, res.Version)
		default:
			fmt.Printf("%s: định nghĩa tool ĐÃ ĐỔI. Version %d chờ duyệt; tool bị tạm khoá: %s\n",
				srv.Slug, res.Version, strings.Join(res.LockedTools, ", "))
			if len(res.KeptDisabled) > 0 {
				fmt.Printf("tool vẫn TẮT ở version mới (giữ lựa chọn trước đó): %s\n", strings.Join(res.KeptDisabled, ", "))
			}
		}

	case "mcp-server review":
		serverFlags("slug")
		version := fs.Int("version", 0, "số version cần duyệt")
		approve := fs.Bool("approve", false, "duyệt version")
		reject := fs.Bool("reject", false, "từ chối version")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *approve == *reject || *version < 1 {
			return errors.New("cần -version N và đúng một trong -approve / -reject")
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		if err := adminops.ReviewMCPServerVersion(ctx, conn, srv, *version, *approve); err != nil {
			return err
		}
		fmt.Printf("%s version %d: approved=%v\n", srv.Slug, *version, *approve)

	case "mcp-server scope":
		serverFlags("slug")
		scopedFlag := fs.String("scoped", "", "true | false: server chỉ dùng được ở project đã gắn")
		addFlag := fs.String("add", "", "slug project cần gắn, phân cách bằng dấu phẩy")
		removeFlag := fs.String("remove", "", "slug project cần gỡ, phân cách bằng dấu phẩy")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var scoped *bool
		switch *scopedFlag {
		case "":
		case "true", "false":
			b := *scopedFlag == "true"
			scoped = &b
		default:
			return errors.New("-scoped phải là true hoặc false")
		}
		split := func(v string) []string {
			var out []string
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		now, bound, err := adminops.ScopeMCPServer(ctx, conn, srv, scoped, split(*addFlag), split(*removeFlag))
		if err != nil {
			return err
		}
		if now {
			fmt.Printf("%s chỉ dùng được ở các project: %s\n", srv.Slug, strings.Join(bound, ", "))
		} else {
			fmt.Printf("%s dùng được ở MỌI project của organization (project đã gắn: %s)\n", srv.Slug, strings.Join(bound, ", "))
		}

	case "mcp-tool classify":
		serverFlags("server")
		tool := fs.String("tool", "", "tên tool")
		enabled := fs.String("enabled", "", "true | false")
		risk := fs.String("risk", "", "read | write | destructive | financial")
		sensitivity := fs.String("sensitivity", "", "public | internal | sensitive (sensitive: chỉ chạy trong execution; execution đã đọc nó gọi tool egress thì cần duyệt)")
		egress := fs.String("egress", "", "true | false (tool gửi dữ liệu ra khỏi platform)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var c registry.Classification
		switch *enabled {
		case "":
		case "true", "false":
			b := *enabled == "true"
			c.Enabled = &b
		default:
			return errors.New("-enabled phải là true hoặc false")
		}
		if *risk != "" {
			c.RiskLevel = risk
		}
		switch *sensitivity {
		case "":
		case "public", "internal", "sensitive":
			c.DataSensitivity = sensitivity
		default:
			return errors.New("-sensitivity phải là public, internal hoặc sensitive")
		}
		switch *egress {
		case "":
		case "true", "false":
			b := *egress == "true"
			c.Egress = &b
		default:
			return errors.New("-egress phải là true hoặc false")
		}
		conn, err := connect()
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		if err := adminops.ClassifyMCPTool(ctx, conn, srv, *tool, c); err != nil {
			return err
		}
		fmt.Printf("đã phân loại %s.%s\n", srv.Slug, *tool)

	default:
		return errors.New(usage)
	}
	return nil
}

// secretsInit tạo khoá gốc của provider khoá cục bộ (ADR-0012) vào một file chỉ chủ
// sở hữu đọc được, và in khoá công khai để cấp cho process api. Khoá gốc không được
// in ra và không ghi đè file đã có.
func secretsInit(args []string) error {
	fs := flag.NewFlagSet("secrets init", flag.ContinueOnError)
	out := fs.String("out", "", "đường dẫn file khoá gốc sẽ tạo (cấp cho gateway qua HQ_SECRETS_ROOT_KEY_FILE)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("cần -out")
	}
	root, pub, err := secrets.GenerateLocalRootKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("không tạo được file khoá gốc (file đã tồn tại?): %w", err)
	}
	if _, err := f.WriteString(root + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("đã tạo khoá gốc: %s (chỉ cấp cho gateway)\nHQ_SECRETS_PUBLIC_KEY=%s\n", *out, pub)
	return nil
}

// serviceKeyInit tạo khoá ký ngữ cảnh của Gateway (seed Ed25519) vào một file mới quyền 0600
// và in khoá công khai để cấu hình cho managed server (HQ_SERVICE_VERIFY_KEY). Seed không
// bao giờ được in ra màn hình.
func serviceKeyInit(args []string) error {
	fs := flag.NewFlagSet("service-key init", flag.ContinueOnError)
	out := fs.String("out", "", "đường dẫn file seed sẽ tạo (nội dung cấp cho gateway và hqadmin qua HQ_SERVICE_SIGNING_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("cần -out")
	}
	seed, pub, err := signedctx.GenerateSigningKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("không tạo được file khoá (file đã tồn tại?): %w", err)
	}
	if _, err := f.WriteString(seed + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("đã tạo khoá ký: %s (chỉ cấp cho gateway và hqadmin)\nHQ_SERVICE_VERIFY_KEY=%s\n", *out, pub)
	return nil
}
