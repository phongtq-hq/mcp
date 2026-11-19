package adminops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
)

// Kiểm tra và xuất audit trail (roadmap Phase 6: "xuất audit ra kho ngoài; kiểm tra chuỗi
// hash định kỳ"). Audit của mỗi org là một chuỗi: hash của mỗi bản ghi phủ hash của bản
// ghi trước và dạng chuẩn hoá của chính nó (hàm audit_canonical, migration 00003).
//
// Khác audit_verify trong database, các hàm ở đây kiểm được chuỗi đã bị cắt đầu bởi
// retention (bản ghi còn lại đầu tiên là mỏ neo: prev_hash của nó được nhận như đã ghi),
// không khoá việc ghi audit của org trong lúc kiểm (một câu SELECT thấy một ảnh chụp, và
// việc ghi được tuần tự hoá theo org nên ảnh chụp luôn là một tiền tố của chuỗi), và
// tạo ra file tự kiểm được mà không cần database.

// ErrAuditChainBroken báo chuỗi hash của audit không còn nguyên vẹn.
var ErrAuditChainBroken = errors.New("chuỗi hash của audit bị đứt")

// AuditChain là kết quả kiểm chuỗi audit của một org.
type AuditChain struct {
	OrgID   uuid.UUID
	OrgSlug string
	Records int64
	// Anchored: bản ghi đầu tiên còn lại không phải bản ghi đầu của chuỗi (phần trước đã
	// bị xoá theo retention); chuỗi được kiểm từ bản ghi đó.
	Anchored bool
	// LastHash là hash của bản ghi cuối đã kiểm (nil khi org chưa có bản ghi nào).
	LastHash []byte
}

// auditRecord là một dòng của file xuất. Canonical là đúng chuỗi byte đã được hash, để
// người giữ file kiểm lại được mà không cần database.
type auditRecord struct {
	ID           uuid.UUID       `json:"id"`
	OccurredAt   time.Time       `json:"occurred_at"`
	ProjectID    *uuid.UUID      `json:"project_id,omitempty"`
	ActorType    string          `json:"actor_type"`
	ActorID      *uuid.UUID      `json:"actor_id,omitempty"`
	OnBehalfOf   *uuid.UUID      `json:"on_behalf_of,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *uuid.UUID      `json:"resource_id,omitempty"`
	Outcome      string          `json:"outcome"`
	ExecutionID  *uuid.UUID      `json:"execution_id,omitempty"`
	Details      json.RawMessage `json:"details"`
	IP           *string         `json:"ip,omitempty"`
	TraceID      *string         `json:"trace_id,omitempty"`
	PrevHash     string          `json:"prev_hash"`
	Hash         string          `json:"hash"`
	Canonical    string          `json:"canonical"`
}

// AuditManifest là dòng cuối của file xuất: nó nói file phủ khoảng nào của chuỗi.
type AuditManifest struct {
	Format        int       `json:"format"`
	OrgID         uuid.UUID `json:"org_id"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	Records       int64     `json:"records"`
	FirstPrevHash string    `json:"first_prev_hash"`
	LastHash      string    `json:"last_hash"`
}

type auditLine struct {
	Record   *auditRecord   `json:"record,omitempty"`
	Manifest *AuditManifest `json:"manifest,omitempty"`
}

const auditQuery = `SELECT a.id, a.occurred_at, a.project_id, a.actor_type, a.actor_id, a.on_behalf_of, a.action, a.resource_type,
       a.resource_id, a.outcome, a.execution_id, a.details, host(a.ip), a.trace_id, a.prev_hash, a.hash, audit_canonical(a)
FROM audit_logs a
WHERE a.org_id = $1 AND a.occurred_at >= $2 AND a.occurred_at < $3
ORDER BY a.occurred_at, a.id`

// walkAudit duyệt chuỗi audit của org trong [from, to) theo thứ tự, kiểm từng mắt xích,
// và gọi emit cho từng bản ghi.
func walkAudit(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, from, to time.Time, emit func(auditRecord) error) (AuditChain, error) {
	chain := AuditChain{OrgID: orgID}
	rows, err := tx.Query(ctx, auditQuery, orgID, from, to)
	if err != nil {
		return chain, err
	}
	defer rows.Close()
	for rows.Next() {
		var r auditRecord
		var prev, hash, canonical []byte
		if err := rows.Scan(&r.ID, &r.OccurredAt, &r.ProjectID, &r.ActorType, &r.ActorID, &r.OnBehalfOf, &r.Action, &r.ResourceType,
			&r.ResourceID, &r.Outcome, &r.ExecutionID, &r.Details, &r.IP, &r.TraceID, &prev, &hash, &canonical); err != nil {
			return chain, err
		}
		if chain.Records == 0 {
			chain.Anchored = prev != nil
		} else if !bytes.Equal(prev, chain.LastHash) {
			return chain, fmt.Errorf("%w: bản ghi %s không nối với bản ghi trước nó (vị trí %d)", ErrAuditChainBroken, r.ID, chain.Records)
		}
		sum := sha256.Sum256(append(append([]byte{}, prev...), canonical...))
		if !bytes.Equal(sum[:], hash) {
			return chain, fmt.Errorf("%w: nội dung của bản ghi %s không khớp hash của nó (vị trí %d)", ErrAuditChainBroken, r.ID, chain.Records)
		}
		r.OccurredAt = r.OccurredAt.UTC()
		r.PrevHash, r.Hash, r.Canonical = hex.EncodeToString(prev), hex.EncodeToString(hash), base64.StdEncoding.EncodeToString(canonical)
		if emit != nil {
			if err := emit(r); err != nil {
				return chain, err
			}
		}
		chain.LastHash = hash
		chain.Records++
	}
	return chain, rows.Err()
}

var (
	auditEpoch   = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	auditForever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

// verifyOrgTimeout là hạn kiểm chuỗi của MỘT org: org đứng sau không mất lượt vì org đứng trước lớn.
const verifyOrgTimeout = 30 * time.Minute

// VerifyAudit kiểm chuỗi audit của một org (orgSlug) hoặc của mọi org (orgSlug rỗng).
// Kết quả gồm cả các org đã kiểm xong trước khi gặp chuỗi đứt; lỗi bọc ErrAuditChainBroken.
//
// Chuỗi không bắt đầu từ bản ghi đầu tiên của nó (Anchored) là chuỗi đã mất phần đầu.
// Điều đó chỉ hợp lệ khi retention đã xoá phần đầu; platform CHƯA xoá audit ở đâu cả, nên
// nó bị coi là chuỗi đứt trừ khi người gọi cho phép tường minh (allowTruncatedHead), và
// khi cho phép thì nên đối chiếu mỏ neo với hash cuối của file đã xuất trước đó.
//
// Không tìm thấy organization nào cũng là lỗi: một lần kiểm không kiểm gì không được
// báo là "sạch" (ví dụ tài khoản kết nối không đọc được bảng organizations).
func VerifyAudit(ctx context.Context, conn *pgxv5.Conn, orgSlug string, allowTruncatedHead bool) ([]AuditChain, error) {
	slugs := []string{orgSlug}
	if orgSlug == "" {
		slugs = slugs[:0]
		// Liệt kê dưới role app_admin, như mọi thao tác xuyên tenant khác.
		err := adminTx(ctx, conn, "", func(tx pgxv5.Tx, _ *uuid.UUID) error {
			rows, err := tx.Query(ctx, `SELECT slug FROM organizations ORDER BY slug`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					return err
				}
				slugs = append(slugs, s)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		if len(slugs) == 0 {
			return nil, errors.New("không thấy organization nào để kiểm: tài khoản kết nối có đọc được bảng organizations không?")
		}
	}
	var out []AuditChain
	for _, slug := range slugs {
		var chain AuditChain
		octx, cancel := context.WithTimeout(ctx, verifyOrgTimeout)
		err := adminTx(octx, conn, slug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
			var err error
			chain, err = walkAudit(octx, tx, *orgID, auditEpoch, auditForever, nil)
			return err
		})
		cancel()
		chain.OrgSlug = slug
		if err != nil {
			return out, fmt.Errorf("organization %s: %w", slug, err)
		}
		if chain.Anchored && !allowTruncatedHead {
			return out, fmt.Errorf("organization %s: %w: chuỗi không bắt đầu từ bản ghi đầu tiên của nó (phần đầu đã bị xoá)", slug, ErrAuditChainBroken)
		}
		out = append(out, chain)
	}
	return out, nil
}

// auditHorizon trả về mốc thời gian mà mọi bản ghi audit của org trước mốc đó đã commit:
// thời điểm của database đọc trong lúc giữ khoá ghi audit của org (khoá được giữ tới khi
// transaction ghi commit, nên không còn bản ghi nào có thời điểm sớm hơn mà chưa hiện ra).
// Khoá chỉ được giữ trong một transaction rất ngắn, không phải suốt lúc xuất.
func auditHorizon(ctx context.Context, conn *pgxv5.Conn, orgSlug string) (time.Time, error) {
	var at time.Time
	err := adminTx(ctx, conn, orgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('audit:' || $1::text, 0))`, orgID.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	})
	return at, err
}

// ExportAudit ghi audit của một org trong [from, to) ra w, mỗi dòng một JSON, kết thúc
// bằng một dòng manifest. Chuỗi được kiểm trong lúc xuất: file không được hoàn tất (không
// có manifest) nếu chuỗi đứt. File chứa đúng những gì audit chứa: không có secret hay nội
// dung prompt, nhưng có định danh người dùng và tài nguyên: nơi nhận phải là kho được bảo
// vệ như dữ liệu audit.
func ExportAudit(ctx context.Context, conn *pgxv5.Conn, orgSlug string, from, to time.Time, w io.Writer) (AuditManifest, error) {
	if orgSlug == "" {
		return AuditManifest{}, errors.New("cần slug của organization")
	}
	if !from.Before(to) {
		return AuditManifest{}, errors.New("from phải trước to")
	}
	// Khoảng xuất phải đã đóng: xuất tới một thời điểm còn nhận bản ghi mới thì file trông
	// hoàn chỉnh mà thiếu bản ghi, và lần xuất kế tiếp (bắt đầu từ to) bỏ sót chúng.
	horizon, err := auditHorizon(ctx, conn, orgSlug)
	if err != nil {
		return AuditManifest{}, err
	}
	if to.After(horizon) {
		return AuditManifest{}, fmt.Errorf("to (%s) ở sau thời điểm hiện tại của database (%s): chỉ xuất khoảng đã đóng",
			to.UTC().Format(time.RFC3339), horizon.UTC().Format(time.RFC3339))
	}
	var m AuditManifest
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	err = adminTx(ctx, conn, orgSlug, func(tx pgxv5.Tx, orgID *uuid.UUID) error {
		first, seen := "", false
		chain, err := walkAudit(ctx, tx, *orgID, from.UTC(), to.UTC(), func(r auditRecord) error {
			if !seen {
				first, seen = r.PrevHash, true
			}
			return enc.Encode(auditLine{Record: &r})
		})
		if err != nil {
			return err
		}
		m = AuditManifest{Format: 1, OrgID: *orgID, From: from.UTC(), To: to.UTC(), Records: chain.Records, FirstPrevHash: first,
			LastHash: hex.EncodeToString(chain.LastHash)}
		return enc.Encode(auditLine{Manifest: &m})
	})
	if err != nil {
		return AuditManifest{}, err
	}
	return m, bw.Flush()
}

// canonicalTime là định dạng thời điểm trong audit_canonical (migration 00003).
const canonicalTime = "2006-01-02T15:04:05.000000Z"

// matchesCanonical báo các trường hiển thị của một bản ghi trong file đúng là nội dung
// đã được hash. MỌI trường được đối chiếu: nếu không, người sửa file đổi được người làm,
// thời điểm hay tài nguyên của một sự kiện mà hash vẫn khớp. Định dạng 1 của
// audit_canonical: [1, id, org_id, project_id, actor_type, actor_id, on_behalf_of, action,
// resource_type, resource_id, outcome, execution_id, details, ip, trace_id, occurred_at].
func matchesCanonical(rec *auditRecord, canonical []byte) (orgID string, ok bool) {
	var c []json.RawMessage
	if json.Unmarshal(canonical, &c) != nil || len(c) != 16 || string(c[0]) != "1" {
		return "", false
	}
	str := func(raw json.RawMessage) *string {
		var v *string
		if json.Unmarshal(raw, &v) != nil {
			bad := "\x00không phải chuỗi"
			return &bad
		}
		return v
	}
	same := func(raw json.RawMessage, want *string) bool {
		got := str(raw)
		if got == nil || want == nil {
			return got == nil && want == nil
		}
		return *got == *want
	}
	id := func(u *uuid.UUID) *string {
		if u == nil {
			return nil
		}
		v := u.String()
		return &v
	}
	recID, at := rec.ID.String(), rec.OccurredAt.UTC().Format(canonicalTime)
	var details, shown bytes.Buffer
	if json.Compact(&details, c[12]) != nil || json.Compact(&shown, rec.Details) != nil {
		return "", false
	}
	org := str(c[2])
	if org == nil {
		return "", false
	}
	return *org, same(c[1], &recID) && same(c[3], id(rec.ProjectID)) && same(c[4], &rec.ActorType) && same(c[5], id(rec.ActorID)) &&
		same(c[6], id(rec.OnBehalfOf)) && same(c[7], &rec.Action) && same(c[8], &rec.ResourceType) && same(c[9], id(rec.ResourceID)) &&
		same(c[10], &rec.Outcome) && same(c[11], id(rec.ExecutionID)) && bytes.Equal(details.Bytes(), shown.Bytes()) &&
		same(c[13], rec.IP) && same(c[14], rec.TraceID) && same(c[15], &at)
}

// VerifyAuditFile kiểm một file do ExportAudit tạo, không cần database: từng bản ghi
// khớp hash của nó; các bản ghi nối nhau và có thời điểm tăng dần; MỌI trường hiển thị
// khớp nội dung đã hash; mọi bản ghi thuộc đúng organization và nằm trong khoảng thời
// gian của manifest; manifest khớp nội dung. Mỗi dòng phải đúng là dạng mà ExportAudit ghi
// ra (không trường lạ, không khoá trùng): một dòng mà hai trình đọc JSON hiểu khác nhau bị
// từ chối. File thiếu manifest (bị cắt) bị từ chối.
//
// Giới hạn: file không có chữ ký. Việc kiểm này bắt được sửa đổi không tính lại hash; ai
// viết lại được cả file thì tính lại được mọi hash và manifest. Bảo đảm chỉ có khi hash
// cuối của mỗi lần xuất được giữ ở nơi người sửa file không với tới (xem VerifyAuditFiles
// để đối chiếu các file liên tiếp, và security §9).
func VerifyAuditFile(r io.Reader) (AuditManifest, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var prev []byte
	var n int64
	var lastAt, firstAt time.Time
	first, org := "", ""
	for sc.Scan() {
		raw := sc.Bytes()
		var line auditLine
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&line); err != nil {
			return AuditManifest{}, fmt.Errorf("%w: dòng %d không đọc được: %w", ErrAuditChainBroken, n+1, err)
		}
		// Ghi lại dòng vừa đọc phải ra đúng dòng trong file: khoá trùng hay cách viết khác
		// của cùng một giá trị không lọt qua.
		var again bytes.Buffer
		if err := json.NewEncoder(&again).Encode(line); err != nil || !bytes.Equal(bytes.TrimRight(again.Bytes(), "\n"), raw) {
			return AuditManifest{}, fmt.Errorf("%w: dòng %d không đúng dạng của file xuất", ErrAuditChainBroken, n+1)
		}
		if line.Manifest != nil {
			m := *line.Manifest
			if line.Record != nil || m.Format != 1 || m.Records != n || m.FirstPrevHash != first || m.LastHash != hex.EncodeToString(prev) ||
				(n > 0 && (m.OrgID.String() != org || firstAt.Before(m.From) || !lastAt.Before(m.To))) {
				return m, fmt.Errorf("%w: manifest không khớp nội dung file (%d bản ghi)", ErrAuditChainBroken, n)
			}
			if sc.Scan() {
				return m, fmt.Errorf("%w: có dữ liệu sau manifest", ErrAuditChainBroken)
			}
			return m, sc.Err()
		}
		rec := line.Record
		if rec == nil {
			return AuditManifest{}, fmt.Errorf("%w: dòng %d không phải bản ghi hay manifest", ErrAuditChainBroken, n+1)
		}
		prevHash, err1 := hex.DecodeString(rec.PrevHash)
		hash, err2 := hex.DecodeString(rec.Hash)
		canonical, err3 := base64.StdEncoding.DecodeString(rec.Canonical)
		if err1 != nil || err2 != nil || err3 != nil {
			return AuditManifest{}, fmt.Errorf("%w: dòng %d: hash hoặc canonical không đọc được", ErrAuditChainBroken, n+1)
		}
		if n == 0 {
			first, firstAt = rec.PrevHash, rec.OccurredAt
		} else if !bytes.Equal(prevHash, prev) || !rec.OccurredAt.After(lastAt) {
			return AuditManifest{}, fmt.Errorf("%w: bản ghi %s không nối với bản ghi trước nó (dòng %d)", ErrAuditChainBroken, rec.ID, n+1)
		}
		sum := sha256.Sum256(append(append([]byte{}, prevHash...), canonical...))
		if !bytes.Equal(sum[:], hash) {
			return AuditManifest{}, fmt.Errorf("%w: bản ghi %s không khớp hash của nó (dòng %d)", ErrAuditChainBroken, rec.ID, n+1)
		}
		recOrg, ok := matchesCanonical(rec, canonical)
		if !ok || (n > 0 && recOrg != org) {
			return AuditManifest{}, fmt.Errorf("%w: các trường của bản ghi %s không khớp nội dung đã hash (dòng %d)", ErrAuditChainBroken, rec.ID, n+1)
		}
		org, prev, lastAt = recOrg, hash, rec.OccurredAt
		n++
	}
	if err := sc.Err(); err != nil {
		return AuditManifest{}, fmt.Errorf("%w: %w", ErrAuditChainBroken, err)
	}
	return AuditManifest{}, fmt.Errorf("%w: file không có manifest (bị cắt?)", ErrAuditChainBroken)
}

// VerifyAuditFiles kiểm file cur và, nếu có prev (file của lần xuất ngay trước), kiểm cả
// prev và việc hai file nối nhau: cùng organization, cur bắt đầu đúng nơi prev kết thúc
// (thời gian và hash). Đây là cách thấy bản ghi bị bỏ sót hay bị xoá giữa hai lần xuất.
func VerifyAuditFiles(prev, cur io.Reader) (AuditManifest, error) {
	m, err := VerifyAuditFile(cur)
	if err != nil || prev == nil {
		return m, err
	}
	p, err := VerifyAuditFile(prev)
	if err != nil {
		return m, fmt.Errorf("file trước: %w", err)
	}
	switch {
	case p.OrgID != m.OrgID:
		return m, fmt.Errorf("%w: hai file thuộc hai organization khác nhau", ErrAuditChainBroken)
	case !p.To.Equal(m.From):
		return m, fmt.Errorf("%w: file sau không bắt đầu ở thời điểm file trước kết thúc", ErrAuditChainBroken)
	case m.Records > 0 && p.Records > 0 && m.FirstPrevHash != p.LastHash:
		return m, fmt.Errorf("%w: bản ghi đầu của file sau không nối với bản ghi cuối của file trước", ErrAuditChainBroken)
	case m.Records > 0 && p.Records == 0 && p.FirstPrevHash != "":
		return m, fmt.Errorf("%w: file trước không hợp lệ", ErrAuditChainBroken)
	}
	return m, nil
}
