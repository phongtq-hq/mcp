import { useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { ApprovalTable } from '@/components/approval-table';
import { Async } from '@/components/async';
import { KV, Page, StatusBadge } from '@/components/ui';
import { t } from '@/i18n';
import { fmtMoney, fmtNumber } from '@/lib/format';

function monthRange(): [string, string] {
  const now = new Date();
  const from = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
  return [from.toISOString(), new Date(now.getTime() + 60_000).toISOString()];
}

async function load() {
  const org = await get<Schemas['Organization']>('/organization');
  const [from, to] = monthRange();
  // Các lời gọi phụ: thiếu quyền đọc một phần thì ô đó hiện "—", không hỏng cả màn hình.
  const [projects, pending, usage, servers, tools] = await Promise.all([
    soft(get<PageOf<Schemas['Project']>>('/projects?limit=200')),
    soft(get<PageOf<Schemas['Approval']>>('/approvals?status=pending&limit=50')),
    soft(get<Schemas['UsageSummary']>(`/usage/summary?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`)),
    soft(get<PageOf<Schemas['McpServer']>>('/mcp-servers?limit=200')),
    soft(get<PageOf<Schemas['McpTool']>>('/mcp-tools?enabled=true&limit=200')),
  ]);
  return { org, projects, pending, usage, servers, tools };
}

function StatLink({ to, label, value, hint }: { to: string; label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <Link className="card stat" to={to}>
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      {hint ? <div className="hint">{hint}</div> : null}
    </Link>
  );
}

export default function OverviewPage() {
  const query = useQuery({ queryKey: ['overview'], queryFn: load });
  return (
    <Page title={t('navOverview')} lead={t('overviewLead')}>
      <Async query={query}>
        {({ org, projects, pending, usage, servers, tools }) => {
          const list = pending ? pending.data : [];
          return (
            <>
              <div className="card">
                <KV pairs={[[t('organization'), `${org.name} (${org.slug})`], [t('status'), <StatusBadge status={org.status} />]]} />
              </div>
              <div className="grid">
                <StatLink to="/projects" label={t('statProjects')} value={projects ? fmtNumber(projects.data.length) : '—'} />
                <StatLink to="/approvals" label={t('statPendingApprovals')} value={pending ? fmtNumber(list.length) : '—'} hint={t('statPendingHint')} />
                <StatLink
                  to="/cost" label={t('statCostMonth')}
                  value={usage ? fmtMoney(usage.totals.estimated_cost) : '—'}
                  hint={usage ? t('statCostHint', { calls: fmtNumber(usage.totals.calls) }) : null}
                />
                <StatLink
                  to="/mcp" label={t('statServers')}
                  value={servers ? fmtNumber(servers.data.length) : '—'}
                  hint={tools ? t('statServersHint', { tools: fmtNumber(tools.data.length) }) : null}
                />
              </div>
              <div>
                <div className="row">
                  <h2>{t('recentApprovals')}</h2>
                  <span className="spacer" />
                  <Link to="/approvals">{t('viewAll')}</Link>
                </div>
                {list.length ? <ApprovalTable rows={list.slice(0, 5)} /> : <div className="card muted">{t('nothingPending')}</div>}
              </div>
            </>
          );
        }}
      </Async>
    </Page>
  );
}
