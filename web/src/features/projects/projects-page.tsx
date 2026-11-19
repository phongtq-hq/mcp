import { useQuery } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router';
import { get, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { EnvLabel } from '@/components/labels';
import { Page } from '@/components/ui';
import { t } from '@/i18n';
import { fmtTime } from '@/lib/format';

export default function ProjectsPage() {
  const navigate = useNavigate();
  const query = useQuery({ queryKey: ['projects'], queryFn: () => get<PageOf<Schemas['Project']>>('/projects?limit=200') });
  return (
    <Page title={t('navProjects')} lead={t('projectsLead')} actions={<Link className="btn primary" to="/projects/new">{t('newProject')}</Link>}>
      <Async query={query}>
        {r => (
          <DataTable
            rows={r.data}
            rowKey={p => p.id}
            onRow={p => navigate('/projects/' + p.id)}
            emptyText={t('noProjects')}
            columns={[
              { label: t('name'), cell: p => <Link to={'/projects/' + p.id} onClick={e => e.stopPropagation()}>{p.name}</Link> },
              { label: t('slug'), cell: p => <span className="mono">{p.slug}</span> },
              { label: t('environment'), cell: p => <EnvLabel environment={p.environment} /> },
              { label: t('createdAt'), className: 'nowrap', cell: p => fmtTime(p.created_at) },
            ]}
          />
        )}
      </Async>
    </Page>
  );
}
