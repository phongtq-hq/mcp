// Màn hình duyệt. Người duyệt quyết định dựa trên đúng những gì hiện ở đây, nên:
// - thiếu nội dung cần duyệt thì nút Duyệt bị khoá (incomplete);
// - tham số hiện đủ, không có khung cuộn che phần dưới (pre.full);
// - ký tự không nhìn thấy hiện thành \uXXXX (visible / json);
// - tham số đã bị che một phần thì có cảnh báo (redactions);
// - màn hình không tự làm mới: nội dung không đổi dưới con trỏ của người duyệt.
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, Navigate, useParams } from 'react-router';
import { api, get, type Schemas } from '@/api/client';
import { pendingApprovalsKey } from '@/api/keys';
import { Async } from '@/components/async';
import { ErrorBox, KV, Notice, RiskBadge, StatusBadge } from '@/components/ui';
import { t } from '@/i18n';
import { incomplete, message, subjectLabel, toolName } from '@/lib/approval';
import { fmtRelative, fmtTime, isUuid, shortId } from '@/lib/format';
import { json, visible } from '@/lib/visible';

type Approval = Schemas['Approval'];
type Decision = 'approve' | 'reject';

function DecisionForm({ approval, blind }: { approval: Approval; blind: boolean }) {
  const client = useQueryClient();
  const [comment, setComment] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const decide = async (decision: Decision) => {
    if (decision === 'approve' && blind) return;
    if (!window.confirm(t(decision === 'approve' ? 'approveConfirm' : 'rejectConfirm'))) return;
    setBusy(true);
    setError(null);
    try {
      const body: { decision: Decision; comment?: string } = { decision };
      if (comment.trim()) body.comment = comment.trim();
      await api<unknown>('POST', `/approvals/${approval.id}/decisions`, body);
      void client.invalidateQueries({ queryKey: pendingApprovalsKey });
      // Đưa truy vấn về trạng thái đầu: màn hình hiện spinner rồi trạng thái mới do API trả về.
      await client.resetQueries({ queryKey: ['approvals', 'detail', approval.id] });
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  return (
    <div className="card form">
      <label>
        {t('comment')}
        <textarea maxLength={2000} value={comment} onChange={e => setComment(e.target.value)} />
      </label>
      <div className="row">
        <button className="primary" disabled={busy || blind} onClick={() => { void decide('approve'); }}>{t('approve')}</button>
        <button className="danger" disabled={busy} onClick={() => { void decide('reject'); }}>{t('reject')}</button>
      </div>
      {error ? <ErrorBox error={error} /> : null}
    </div>
  );
}

export function ApprovalDetail({ approval: a }: { approval: Approval }) {
  const p = a.payload || {};
  const args = p.arguments;
  const blind = incomplete(a);
  const redactions = typeof p.redactions === 'number' ? p.redactions : 0;
  const decisions = a.decisions || [];
  return (
    <>
      <div className="crumbs"><Link to="/approvals">{t('navApprovals')}</Link> / <span className="mono">{shortId(a.id)}</span></div>
      <div className="page-head">
        <div>
          <h1>{t('approvalDetail')} <StatusBadge status={a.status} /> <RiskBadge risk={a.risk_level} /></h1>
          <p className="sub">{subjectLabel(a)}</p>
        </div>
      </div>
      <div className="stack">
        <div className="card">
          <h2>{t('whatIsAsked')}</h2>
          {blind ? <Notice kind="bad">{t('approvalIncomplete')}</Notice> : null}
          {a.subject_type === 'workflow_step' && message(a) ? <div className="answer">{visible(message(a))}</div> : null}
          {a.subject_type === 'tool_call' ? <KV pairs={[[t('tool'), <span className="mono">{toolName(a) || '—'}</span>]]} /> : null}
          {/* Tham số có chỗ đã bị che (mẫu secret): người duyệt KHÔNG thấy toàn bộ hành động. */}
          {redactions > 0 ? <Notice kind="warn">{t('redactedArgs', { n: redactions })}</Notice> : null}
          {args !== undefined && args !== null ? <><div className="small muted">{t('arguments')}</div><pre className="full">{json(args)}</pre></> : null}
          <details>
            <summary className="small muted">{t('rawPayload')}</summary>
            <pre>{json(p)}</pre>
          </details>
        </div>
        <div className="card">
          <KV pairs={[
            [t('requiredApprovals'), a.required_approvals],
            [t('decisions'), decisions.length === 0 ? t('none') : <pre>{json(decisions)}</pre>],
            [t('requestedBy'), <span className="mono">{a.requested_by ? shortId(a.requested_by) : '—'}</span>],
            [t('expiresAt'), `${fmtTime(a.expires_at)} (${fmtRelative(a.expires_at)})`],
            [t('createdAt'), fmtTime(a.created_at)],
            a.execution_id ? [t('execution'), <Link to={'/executions/' + a.execution_id}>{t('viewExecution')}</Link>] : null,
          ]} />
        </div>
        {a.status === 'pending' ? <DecisionForm approval={a} blind={blind} /> : null}
      </div>
    </>
  );
}

export default function ApprovalDetailPage() {
  const { approvalId } = useParams();
  const valid = isUuid(approvalId);
  // Mỗi lần màn hình được dựng có một khoá truy vấn riêng: nó không bao giờ hiện bản đã
  // tải từ trước (mở lại, quay lại, đổi ngôn ngữ) mà luôn tải trạng thái hiện tại rồi mới
  // hiện gì. gcTime 0 để bản của lần dựng trước không nằm lại trong bộ nhớ.
  const [mount] = useState(() => crypto.randomUUID());
  const query = useQuery({
    queryKey: ['approvals', 'detail', approvalId, mount],
    queryFn: () => get<Approval>('/approvals/' + approvalId),
    enabled: valid,
    gcTime: 0,
  });
  if (!valid) return <Navigate to="/" replace />;
  // key theo id: ghi chú đang gõ và trạng thái đang gửi không thể mang sang approval khác.
  return <Async query={query}>{a => <ApprovalDetail key={a.id} approval={a} />}</Async>;
}
