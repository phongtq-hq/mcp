// Danh mục Agent của cả organization, xem theo từng dự án (API liệt kê Agent theo dự án; chưa
// có endpoint liệt kê xuyên dự án). Tìm, lọc và sắp xếp do máy chủ làm; danh sách phân trang
// keyset. Bấm một dòng mở inspector bên phải mà không rời danh sách.
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import { get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { pages, rows } from '@/api/paging';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { Inspector, InspectorSection } from '@/components/inspector';
import { EnvLabel } from '@/components/labels';
import { LoadMore } from '@/components/load-more';
import { Badge, Empty, KV, Page, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtMoney, fmtRelative, fmtTime, isUuid, shortId } from '@/lib/format';
import { useDebounced } from '@/lib/use-debounced';
import { useStored } from '@/lib/use-stored';
import { visible } from '@/lib/visible';

type Agent = Schemas['Agent'];
type Project = Schemas['Project'];

const STATUSES = ['draft', 'active', 'disabled', 'archived'] as const;
const SORTS = ['name', '-name', '-created_at', 'created_at'] as const;

function AgentInspector({ agent, project, onClose }: { agent: Agent; project: Project; onClose: () => void }) {
  const version = agent.active_version ?? agent.latest_version ?? null;
  const detail = useQuery({
    queryKey: ['agent', agent.id, 'version', version],
    queryFn: () => soft(get<Schemas['AgentVersion']>(`/agents/${agent.id}/versions/${version}`)),
    enabled: version !== null,
  });
  const servers = useQuery({ queryKey: ['mcp-servers', 'names'], queryFn: () => soft(get<PageOf<Schemas['McpServer']>>('/mcp-servers?limit=200')) });
  const recent = useQuery({
    queryKey: ['agent', agent.id, 'recent-executions'],
    queryFn: () => soft(get<PageOf<Schemas['Execution']>>(`/projects/${agent.project_id}/executions?agent_id=${agent.id}&limit=5`)),
  });
  const cfg = detail.data?.config;
  const serverName = new Map((servers.data?.data ?? []).map(s => [s.id, s.name]));
  return (
    <Inspector icon="bot" title={agent.name} subtitle={<span className="mono">{agent.slug}</span>} onClose={onClose}>
      <InspectorSection title={t('mapOverview')}>
        {agent.description ? <p className="answer small">{visible(agent.description)}</p> : null}
        <KV pairs={[
          [t('status'), <StatusBadge status={agent.status} />],
          [t('project'), <Link to={`/projects/${project.id}/agents`}>{project.name}</Link>],
          [t('environment'), <EnvLabel environment={project.environment} />],
          [t('activeVersion'), agent.active_version ?? t('notPublished')],
          [t('latestVersion'), agent.latest_version ?? '—'],
          [t('createdAt'), fmtTime(agent.created_at)],
        ]} />
      </InspectorSection>
      <InspectorSection title={t('agentConfigOf', { version: version ?? '—' })}>
        {version === null ? <p className="small muted">{t('notPublished')}</p> : detail.isPending ? <p className="small muted">{t('loading')}</p> : cfg ? (
          <KV pairs={[
            [t('mapModelPolicy'), cfg.model.policy === 'tiered' ? t('mapPolicyTiered', { tier: cfg.model.default_tier ?? 'standard' }) : t('mapPolicyFixed')],
            [t('mapLimits'), t('mapLimitsValue', { iterations: cfg.limits.max_iterations, calls: cfg.limits.max_tool_calls, seconds: cfg.limits.timeout_seconds })],
            cfg.limits.budget ? [t('mapBudget'), fmtMoney(cfg.limits.budget)] : null,
            cfg.delegation ? [t('mapDelegation'), t('agentDelegates', { count: cfg.delegation.can_delegate_to?.length ?? 0, depth: cfg.delegation.max_depth })] : null,
          ]} />
        ) : <p className="small muted">{t('mapNoConfig')}</p>}
      </InspectorSection>
      {cfg ? (
        <InspectorSection title={t('mapPermissions')}>
          {cfg.tools.length === 0 ? <p className="small muted">{t('mapNoServers')}</p> : (
            <ul className="inspector-list">
              {cfg.tools.map((ref, i) => (
                <li key={i} className="wrap">
                  <strong>{serverName.get(ref.server_id) ?? shortId(ref.server_id)}</strong>
                  {ref.require_approval === 'always' ? <Badge tone="warn">{t('mapAlwaysApproval')}</Badge> : null}
                  <div className="small"><span className="muted">{t('access_allow')}: </span><span className="mono">{ref.allow.join(', ') || '—'}</span></div>
                  {ref.deny?.length ? <div className="small"><span className="muted">{t('access_deny')}: </span><span className="mono">{ref.deny.join(', ')}</span></div> : null}
                </li>
              ))}
            </ul>
          )}
        </InspectorSection>
      ) : null}
      <InspectorSection title={t('mapRecentExecutions')}>
        {recent.isPending ? <p className="small muted">{t('loading')}</p> : recent.data ? (
          recent.data.data.length === 0 ? <p className="small muted">{t('mapNoExecutions')}</p> : (
            <ul className="inspector-list">
              {recent.data.data.map(x => (
                <li key={x.id}>
                  <Link className="mono" to={'/executions/' + x.id}>{shortId(x.id)}</Link>
                  <StatusBadge status={x.status} />
                  <span className="spacer" />
                  <span className="small muted">{fmtRelative(x.created_at)}</span>
                </li>
              ))}
            </ul>
          )
        ) : <p className="small muted">{t('mapNoAccess')}</p>}
      </InspectorSection>
    </Inspector>
  );
}

function Inventory({ projects }: { projects: Project[] }) {
  const [storedProject, setProject] = useStored('hq.agents.project', '');
  const [status, setStatus] = useStored('hq.agents.status', '');
  const [sort, setSort] = useStored('hq.agents.sort', 'name');
  const [text, setText] = useState('');
  const [open, setOpen] = useState<string | null>(null);
  const q = useDebounced(text.trim());

  const project = projects.find(p => p.id === storedProject) ?? projects[0];
  const projectId = project && isUuid(project.id) ? project.id : '';
  const statusFilter = (STATUSES as readonly string[]).includes(status) ? status : '';
  const sortBy = (SORTS as readonly string[]).includes(sort) ? sort : 'name';
  const path = `/projects/${projectId}/agents?limit=50&sort=${encodeURIComponent(sortBy)}`
    + (statusFilter ? '&status=' + statusFilter : '') + (q ? '&q=' + encodeURIComponent(q) : '');
  const list = useInfiniteQuery({
    queryKey: ['project', projectId, 'agents', 'inventory', statusFilter, sortBy, q],
    ...pages<Agent>(path),
    enabled: projectId !== '',
  });
  const all = rows(list.data);
  const selected = all.find(a => a.id === open);
  const filtered = statusFilter !== '' || q !== '';

  if (!project) return <Empty hint={t('noProjectsHint')} action={<Link className="btn primary" to="/projects/new">{t('newProject')}</Link>}>{t('noProjects')}</Empty>;

  return (
    <div className="with-inspector">
      <div className="stack">
        <div className="row filters">
          <label className="inline">
            <span className="small muted">{t('project')}</span>
            <select value={project.id} onChange={e => { setProject(e.target.value); setOpen(null); }}>
              {projects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
            </select>
          </label>
          <label className="inline">
            <span className="small muted">{t('status')}</span>
            <select value={statusFilter} onChange={e => setStatus(e.target.value)}>
              <option value="">{t('filterAllStatuses')}</option>
              {STATUSES.map(s => <option key={s} value={s}>{tOr('st_' + s, s)}</option>)}
            </select>
          </label>
          <label className="inline">
            <span className="small muted">{t('sortBy')}</span>
            <select value={sortBy} onChange={e => setSort(e.target.value)}>
              {SORTS.map(s => <option key={s} value={s}>{t(`agentSort_${s}`)}</option>)}
            </select>
          </label>
          <label className="inline grow">
            <span className="small muted">{t('search')}</span>
            <input type="search" value={text} placeholder={t('agentSearchPlaceholder')} onChange={e => setText(e.target.value)} />
          </label>
          <EnvLabel environment={project.environment} />
        </div>
        <Async query={list}>
          {() => (
            <>
              <DataTable
                rows={all}
                rowKey={a => a.id}
                onRow={a => setOpen(a.id)}
                emptyText={filtered ? t('noAgentsMatch') : t('noAgents')}
                emptyHint={filtered ? null : t('noAgentsHint')}
                columns={[
                  { label: t('name'), cell: a => <><div>{a.name}</div>{a.description ? <div className="small muted clip">{a.description}</div> : null}</> },
                  { label: t('slug'), cell: a => <span className="mono">{a.slug}</span> },
                  { label: t('status'), cell: a => <StatusBadge status={a.status} /> },
                  { label: t('activeVersion'), className: 'num', cell: a => a.active_version ?? <span className="muted">{t('notPublished')}</span> },
                  { label: t('latestVersion'), className: 'num', cell: a => a.latest_version },
                  { label: t('updatedAt'), className: 'nowrap', cell: a => fmtRelative(a.updated_at) },
                ]}
              />
              <LoadMore query={list} />
            </>
          )}
        </Async>
      </div>
      {selected ? <AgentInspector key={selected.id} agent={selected} project={project} onClose={() => setOpen(null)} /> : null}
    </div>
  );
}

export default function AgentsPage() {
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => get<PageOf<Project>>('/projects?limit=200') });
  return (
    <Page title={t('navAgents')} lead={t('agentsLead')} actions={<Link className="btn" to="/agents/map"><span>{t('navAgentMap')}</span></Link>}>
      <Async query={projects}>{r => <Inventory projects={r.data} />}</Async>
    </Page>
  );
}
