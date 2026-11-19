import { useQuery } from '@tanstack/react-query';
import { get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { Badge, Page, RiskBadge, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { shortId } from '@/lib/format';

async function load() {
  const [servers, tools] = await Promise.all([
    get<PageOf<Schemas['McpServer']>>('/mcp-servers?limit=200'),
    soft(get<PageOf<Schemas['McpTool']>>('/mcp-tools?limit=200')),
  ]);
  return { servers, tools };
}

export default function McpPage() {
  const query = useQuery({ queryKey: ['mcp'], queryFn: load });
  return (
    <Page title={t('navMcp')} lead={t('mcpLead')}>
      <Async query={query}>
        {({ servers, tools }) => {
          const serverName = new Map(servers.data.map(s => [s.id, s.slug]));
          return (
            <>
              <div>
                <h2>{t('servers')}</h2>
                <DataTable
                  rows={servers.data}
                  rowKey={s => s.id}
                  emptyText={t('noServers')}
                  columns={[
                    { label: t('name'), cell: s => <><div>{s.name}</div><div className="small mono muted">{s.slug}</div></> },
                    { label: t('kind'), cell: s => tOr('kind_' + s.kind, s.kind) },
                    { label: t('scope'), cell: s => tOr('scope_' + s.scope, s.scope ?? '') },
                    { label: t('status'), cell: s => <><StatusBadge status={s.status} />{s.pending_review_version ? <> <Badge tone="warn">{t('pendingReview')}</Badge></> : null}</> },
                    { label: t('version'), className: 'num', cell: s => s.active_version ?? '—' },
                  ]}
                />
              </div>
              <div>
                <h2>{t('tools')}</h2>
                <DataTable
                  rows={tools ? tools.data : []}
                  rowKey={x => x.id}
                  emptyText={t('noTools')}
                  columns={[
                    { label: t('tool'), cell: x => <><div className="mono">{`${serverName.get(x.server_id) || shortId(x.server_id)}.${x.name}`}</div><div className="small muted">{x.description}</div></> },
                    { label: t('risk'), cell: x => <RiskBadge risk={x.risk_level} /> },
                    { label: t('sideEffect'), cell: x => tOr('se_' + x.side_effect, x.side_effect) },
                    { label: t('status'), cell: x => x.enabled ? <Badge tone="ok">{t('enabled')}</Badge> : <Badge>{t('disabledTool')}</Badge> },
                  ]}
                />
              </div>
            </>
          );
        }}
      </Async>
    </Page>
  );
}
