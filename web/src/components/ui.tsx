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

// Trạng thái phân biệt được bằng HÌNH, không chỉ bằng màu: dấu tích (xong), dấu chéo (lỗi),
// hai vạch (đang chờ người hoặc tạm dừng), vòng (đang chạy; quay khi `busy`), chấm (còn lại).
export function StatusIcon({ tone, busy }: { tone?: string; busy?: boolean }) {
  const common = { className: 'status-icon' + (busy ? ' spin' : ''), viewBox: '0 0 16 16', 'aria-hidden': true, fill: 'none', stroke: 'currentColor', strokeWidth: 2.2, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const };
  if (tone === 'ok') return <svg {...common}><path d="M3 8.5l3.2 3.2L13 4.8" /></svg>;
  if (tone === 'bad') return <svg {...common}><path d="M4 4l8 8M12 4l-8 8" /></svg>;
  if (tone === 'warn') return <svg {...common}><path d="M5.5 3.5v9M10.5 3.5v9" /></svg>;
  if (tone === 'info') return <svg {...common}><path d="M8 2.5a5.5 5.5 0 1 0 5.5 5.5" /></svg>;
  return <svg {...common}><circle cx="8" cy="8" r="2.2" fill="currentColor" stroke="none" /></svg>;
}

const BUSY: ReadonlySet<string> = new Set(['running', 'started']);

export function StatusBadge({ status }: { status: string | null | undefined }) {
  const tone = status ? TONE[status] : undefined;
  return <Badge tone={tone}><StatusIcon tone={tone} busy={!!status && BUSY.has(status)} />{status ? tOr('st_' + status, status) : t('unknown')}</Badge>;
}

const RISK_TONE: Record<string, string> = { read: 'ok', write: 'warn', destructive: 'bad', financial: 'bad' };

export function RiskBadge({ risk }: { risk: string | null | undefined }) {
  return <Badge tone={risk ? RISK_TONE[risk] : undefined}>{risk ? tOr('risk_' + risk, risk) : t('unknown')}</Badge>;
}

export function Spinner() {
  return <div className="spinner" role="status" aria-label={t('loading')} />;
}

// Empty: trạng thái rỗng. Ngoài câu "chưa có gì", nó nói bước tiếp theo (`hint`) và đưa luôn
// nút làm bước đó (`action`) khi giao diện làm được.
export function Empty({ children, hint, action }: { children: ReactNode; hint?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <svg viewBox="0 0 56 56" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <rect x="9" y="13" width="38" height="30" rx="5" />
        <path d="M9 22h38M18 31h20M18 36h12" />
      </svg>
      <div>{children}</div>
      {hint ? <div className="hint">{hint}</div> : null}
      {action ? <div className="row">{action}</div> : null}
    </div>
  );
}

export type NoticeKind = 'ok' | 'warn' | 'bad';

export function Notice({ kind, children }: { kind: NoticeKind; children: ReactNode }) {
  return <div className={'notice ' + kind} role={kind === 'bad' ? 'alert' : 'status'}>{children}</div>;
}

// Success: kết quả thành công của một thao tác, hiện ngay cạnh chỗ vừa bấm. Có dấu tích (hình,
// không chỉ màu) và được trình đọc màn hình đọc lên (role="status" của Notice).
export function Success({ children }: { children: ReactNode }) {
  return <Notice kind="ok"><span className="outcome"><StatusIcon tone="ok" />{children}</span></Notice>;
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
