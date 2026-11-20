import { useQuery } from '@tanstack/react-query';
import { Link, Navigate, Outlet, useLocation, useParams } from 'react-router';
import { get, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { EnvLabel } from '@/components/labels';
import { t, type Key } from '@/i18n';
import { isUuid } from '@/lib/format';
import { useTitle } from '@/lib/use-title';

const TABS: readonly (readonly [string, Key])[] = [['executions', 'tabExecutions'], ['agents', 'tabAgents'], ['workflows', 'tabWorkflows'], ['settings', 'tabSettings']];

// Khung của một dự án: tiêu đề và các tab. Nội dung từng tab là route con (Outlet).
export default function ProjectLayout() {
  const { projectId } = useParams();
  const location = useLocation();
  const valid = isUuid(projectId);
  const query = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => get<Schemas['Project']>('/projects/' + projectId),
    enabled: valid,
  });
  useTitle(query.data?.name);
  if (!valid) return <Navigate to="/" replace />;
  const last = location.pathname.split('/').filter(Boolean)[2];
  const tab = TABS.some(([key]) => key === last) ? last : 'executions';
  return (
    <Async loading="detail" query={query} back={<Link className="btn" to="/projects">{t('navProjects')}</Link>}>
      {project => (
        <>
          <div className="crumbs"><Link to="/projects">{t('navProjects')}</Link> / {project.name}</div>
          <div className="page-head">
            <div>
              <h1>{project.name}</h1>
              <p className="sub"><span className="mono">{project.slug}</span> · <EnvLabel environment={project.environment} /></p>
            </div>
          </div>
          <div className="tabs" role="tablist">
            {TABS.map(([key, label]) => (
              <Link key={key} to={`/projects/${project.id}/${key}`} role="tab" aria-current={key === tab ? 'page' : undefined}>{t(label)}</Link>
            ))}
          </div>
          <div className="stack"><Outlet /></div>
        </>
      )}
    </Async>
  );
}
