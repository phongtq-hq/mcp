import { useNavigate } from 'react-router';
import { t } from '@/i18n';
import { subjectLabel, subjectText, type ApprovalLike } from '@/lib/approval';
import { fmtRelative, fmtTime } from '@/lib/format';
import { DataTable } from './data-table';
import { RiskBadge, StatusBadge } from './ui';

export interface ApprovalRow extends ApprovalLike {
  id: string;
  risk_level: string;
  status: string;
  required_approvals: number;
  decisions?: unknown[];
  expires_at: string;
  resolved_at?: string | null;
}

// Bảng approval dùng ở Tổng quan và ở màn hình Phê duyệt.
export function ApprovalTable({ rows }: { rows: ApprovalRow[] }) {
  const navigate = useNavigate();
  return (
    <DataTable
      rows={rows}
      rowKey={a => a.id}
      onRow={a => navigate('/approvals/' + a.id)}
      emptyText={t('noApprovals')}
      columns={[
        { label: t('subject'), cell: a => <><div className="small muted">{subjectLabel(a)}</div><div className="clip">{subjectText(a)}</div></> },
        { label: t('risk'), cell: a => <RiskBadge risk={a.risk_level} /> },
        { label: t('status'), cell: a => <StatusBadge status={a.status} /> },
        { label: t('decisions'), className: 'num', cell: a => t('decisionsCount', { n: (a.decisions || []).length, need: a.required_approvals }) },
        { label: t('expiresAt'), className: 'nowrap', cell: a => a.status === 'pending' ? fmtRelative(a.expires_at) : fmtTime(a.resolved_at || a.expires_at) },
      ]}
    />
  );
}
