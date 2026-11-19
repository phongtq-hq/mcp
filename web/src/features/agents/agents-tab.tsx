import { useQuery } from '@tanstack/react-query';
import { useParams } from 'react-router';
import { get, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { StatusBadge } from '@/components/ui';
import { t } from '@/i18n';

// Tab Agent của một dự án. projectId đã được khung dự án kiểm dạng.
export default function AgentsTab() {
  const { projectId } = useParams();
  const query = useQuery({
    queryKey: ['project', projectId, 'agents'],
    queryFn: () => get<PageOf<Schemas['Agent']>>(`/projects/${projectId}/agents?limit=200`),
  });
  return (
    <Async query={query}>
      {r => (
        <DataTable
          rows={r.data}
          rowKey={a => a.id}
          emptyText={t('noAgents')}
          columns={[
            { label: t('name'), cell: a => <><div>{a.name}</div>{a.description ? <div className="small muted">{a.description}</div> : null}</> },
            { label: t('slug'), cell: a => <span className="mono">{a.slug}</span> },
            { label: t('status'), cell: a => <StatusBadge status={a.status} /> },
            { label: t('activeVersion'), className: 'num', cell: a => a.active_version ?? <span className="muted">{t('notPublished')}</span> },
            { label: t('latestVersion'), className: 'num', cell: a => a.latest_version },
          ]}
        />
      )}
    </Async>
  );
}
