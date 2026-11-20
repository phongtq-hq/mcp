// Thành phần trình bày dùng chung. Mọi dữ liệu vào trang qua text node của React; không
// có đường nào đưa HTML thô vào DOM.
import type { ReactNode } from 'react';
import { t, tOr } from '@/i18n';
import { ApiError } from '@/lib/api-error';
import { useTitle } from '@/lib/use-title';

const TONE: Record<string, string> = {
  completed: 'ok', succeeded: 'ok', approved: 'ok', consumed: 'ok', active: 'ok', healthy: 'ok', success: 'ok',
  running: 'info', started: 'info', queued: 'info',
  waiting_approval: 'warn', paused: 'warn', pending: 'warn', pending_review: 'warn', budget_exceeded: 'warn', timed_out: 'warn', expired: 'warn',
  failed: 'bad', denied: 'bad', rejected: 'bad', unknown: 'bad', unhealthy: 'bad', error: 'bad',
};

export function Badge({ tone, children }: { tone?: string; children: ReactNode }) {
  return <span className={'badge ' + (tone ?? '')}>{children}</span>;
}

export function StatusBadge({ status }: { status: string | null | undefined }) {
  return <Badge tone={status ? TONE[status] : undefined}>{status ? tOr('st_' + status, status) : t('unknown')}</Badge>;
}

const RISK_TONE: Record<string, string> = { read: 'ok', write: 'warn', destructive: 'bad', financial: 'bad' };

export function RiskBadge({ risk }: { risk: string | null | undefined }) {
  return <Badge tone={risk ? RISK_TONE[risk] : undefined}>{risk ? tOr('risk_' + risk, risk) : t('unknown')}</Badge>;
}

export function Spinner() {
  return <div className="spinner" role="status" aria-label={t('loading')} />;
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export type NoticeKind = 'ok' | 'warn' | 'bad';

export function Notice({ kind, children }: { kind: NoticeKind; children: ReactNode }) {
  return <div className={'notice ' + kind} role={kind === 'bad' ? 'alert' : 'status'}>{children}</div>;
}

export function ErrorBox({ error }: { error: unknown }) {
  let text = error instanceof Error && error.message ? error.message : String(error);
  if (error instanceof ApiError) {
    if (error.status === 403) text = t('errForbidden') + (error.message ? ' (' + error.message + ')' : '');
    else if (error.status === 404) text = t('errNotFound');
  }
  const trace = error instanceof ApiError ? error.traceId : undefined;
  return (
    <Notice kind="bad">
      <strong>{t('errTitle')}: </strong>{text}
      {trace ? <div className="small">{t('errTrace')}: <span className="mono">{trace}</span></div> : null}
    </Notice>
  );
}

// KV: danh sách nhãn / giá trị. Mục null bị bỏ qua.
export function KV({ pairs }: { pairs: (readonly [string, ReactNode] | null)[] }) {
  return (
    <dl className="kv">
      {pairs.map(pair => pair && [
        <dt key={pair[0] + ':k'}>{pair[0]}</dt>,
        <dd key={pair[0] + ':v'}>{pair[1] ?? '—'}</dd>,
      ])}
    </dl>
  );
}

export function PageHead({ title, lead, actions }: { title: ReactNode; lead?: ReactNode; actions?: ReactNode }) {
  useTitle(typeof title === 'string' ? title : null);
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {lead ? <p className="sub">{lead}</p> : null}
      </div>
      <div className="row">{actions}</div>
    </div>
  );
}

export function Page({ title, lead, actions, children }: { title: ReactNode; lead?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <>
      <PageHead title={title} lead={lead} actions={actions} />
      <div className="stack">{children}</div>
    </>
  );
}

export function StatCard({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <div className="card stat">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      {hint ? <div className="hint">{hint}</div> : null}
    </div>
  );
}
