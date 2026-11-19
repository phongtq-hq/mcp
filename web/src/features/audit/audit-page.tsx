import { useQuery } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { get, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { Badge, Page } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtTime, shortId } from '@/lib/format';
import { store, stored } from '@/lib/storage';
import { json } from '@/lib/visible';

const ACTION_KEY = 'hq.audit.action';
const TONE: Record<string, string> = { success: 'ok', denied: 'warn', error: 'bad' };

export default function AuditPage() {
  const [action, setAction] = useState(() => stored(ACTION_KEY, ''));
  const [draft, setDraft] = useState(action);
  const query = useQuery({
    queryKey: ['audit', action],
    queryFn: () => get<PageOf<Schemas['AuditLog']>>('/audit-logs?limit=100' + (action ? '&action=' + encodeURIComponent(action) : '')),
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = draft.trim();
    store(ACTION_KEY, next);
    // Cùng bộ lọc: nút này là "Làm mới".
    if (next === action) void query.refetch();
    else setAction(next);
  };

  const form = (
    <form className="row" onSubmit={submit}>
      <input type="search" size={34} value={draft} placeholder={t('filterAction')} aria-label={t('filterAction')} onChange={e => setDraft(e.target.value)} />
      <button type="submit">{t('refresh')}</button>
    </form>
  );

  return (
    <Page title={t('navAudit')} lead={t('auditLead')} actions={form}>
      <Async query={query}>
        {r => (
          <DataTable
            rows={r.data}
            rowKey={a => a.id}
            emptyText={t('noAudit')}
            columns={[
              { label: t('time'), className: 'nowrap', cell: a => fmtTime(a.occurred_at) },
              { label: t('actor'), cell: a => <>{tOr('actor_' + a.actor_type, a.actor_type)}{a.actor_id ? <> <span className="mono muted">{shortId(a.actor_id)}</span></> : null}</> },
              { label: t('action'), cell: a => <span className="mono">{a.action}</span> },
              { label: t('resource'), cell: a => <>{a.resource_type}{a.resource_id ? <> <span className="mono muted">{shortId(a.resource_id)}</span></> : null}</> },
              { label: t('outcome'), cell: a => <Badge tone={TONE[a.outcome]}>{tOr('out_' + a.outcome, a.outcome)}</Badge> },
              {
                label: t('details'),
                cell: a => a.details && Object.keys(a.details).length
                  ? <details><summary className="small muted">{t('details')}</summary><pre>{json(a.details)}</pre></details>
                  : '—',
              },
            ]}
          />
        )}
      </Async>
    </Page>
  );
}
