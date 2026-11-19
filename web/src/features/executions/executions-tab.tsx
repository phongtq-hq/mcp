import { useQuery } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { api, get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { ErrorBox, StatusBadge } from '@/components/ui';
import { t } from '@/i18n';
import { fmtMoney, fmtTime, shortId } from '@/lib/format';
import { messageOf, modeLabel, TERMINAL } from './shared';

type Agent = Schemas['Agent'];
type Execution = Schemas['Execution'];

async function load(projectId: string) {
  const [list, agents] = await Promise.all([
    get<PageOf<Execution>>(`/projects/${projectId}/executions?limit=50`),
    soft(get<PageOf<Agent>>(`/projects/${projectId}/agents?limit=200`)),
  ]);
  return { list, agents };
}

function RunForm({ projectId, agents }: { projectId: string; agents: Agent[] }) {
  const navigate = useNavigate();
  const [agentId, setAgentId] = useState(agents[0]?.id ?? '');
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (agents.length === 0) return <div className="card muted">{t('noActiveAgents')}</div>;
  // Danh sách Agent được tải lại định kỳ: Agent đang chọn có thể đã biến mất khỏi danh sách.
  const selected = agents.some(a => a.id === agentId) ? agentId : agents[0]?.id ?? '';

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const message = text.trim();
    if (!message || !selected || busy) return;
    setBusy(true);
    setError(null);
    try {
      const x = await api<Execution>('POST', `/projects/${projectId}/executions`, { mode: 'single_agent', agent_id: selected, input: { message } });
      navigate('/executions/' + x.id);
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  return (
    <form className="card form" onSubmit={e => { void submit(e); }}>
      <div><strong>{t('newExecution')}</strong><div className="small muted">{t('newExecutionLead')}</div></div>
      <label>
        {t('chooseAgent')}
        <select required value={selected} onChange={e => setAgentId(e.target.value)}>
          {agents.map(a => <option key={a.id} value={a.id}>{a.name}</option>)}
        </select>
      </label>
      <label>
        {t('request')}
        <textarea required maxLength={8000} placeholder={t('requestPlaceholder')} value={text} onChange={e => setText(e.target.value)} />
      </label>
      <div className="row"><button className="primary" type="submit" disabled={busy}>{busy ? t('running') : t('run')}</button></div>
      {error ? <ErrorBox error={error} /> : null}
    </form>
  );
}

// Tab lần chạy của một dự án. projectId đã được khung dự án kiểm dạng.
export default function ExecutionsTab() {
  const { projectId = '' } = useParams();
  const navigate = useNavigate();
  const query = useQuery({
    queryKey: ['project', projectId, 'executions'],
    queryFn: () => load(projectId),
    // Còn lần chạy chưa kết thúc thì tự làm mới.
    refetchInterval: q => (q.state.data?.list.data.some(x => !TERMINAL.has(x.status)) ? 5000 : false),
  });
  return (
    <Async query={query}>
      {({ list, agents }) => (
        <>
          <RunForm projectId={projectId} agents={(agents ? agents.data : []).filter(a => a.status === 'active' && a.active_version)} />
          <DataTable
            rows={list.data}
            rowKey={x => x.id}
            onRow={x => navigate('/executions/' + x.id)}
            emptyText={t('noExecutions')}
            columns={[
              { label: t('execution'), cell: x => <Link to={'/executions/' + x.id} className="mono" onClick={e => e.stopPropagation()}>{shortId(x.id)}</Link> },
              { label: t('status'), cell: x => <StatusBadge status={x.status} /> },
              { label: t('mode'), cell: x => modeLabel(x.mode) },
              { label: t('request'), cell: x => <div className="clip" title={messageOf(x.input)}>{messageOf(x.input) || '—'}</div> },
              { label: t('cost'), className: 'num', cell: x => fmtMoney(x.estimated_cost) },
              { label: t('createdAt'), className: 'nowrap', cell: x => fmtTime(x.created_at) },
            ]}
          />
        </>
      )}
    </Async>
  );
}
