import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { api, get, type Page as PageOf, type Schemas } from '@/api/client';
import { pages, rows } from '@/api/paging';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { LoadMore } from '@/components/load-more';
import { ErrorBox, StatusBadge } from '@/components/ui';
import { t } from '@/i18n';
import { fmtMoney, fmtTime, isUuid, shortId } from '@/lib/format';
import { useStored } from '@/lib/use-stored';
import { messageOf, modeLabel, TERMINAL } from './shared';

type Agent = Schemas['Agent'];
type Execution = Schemas['Execution'];

// Trạng thái lọc được, theo thứ tự một lần chạy đi qua.
const STATUSES = ['queued', 'running', 'waiting_approval', 'paused', 'completed', 'failed', 'cancelled', 'budget_exceeded', 'timed_out'] as const;

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
  // Form chạy mới chỉ mở khi bấm: người vào để xem lịch sử thấy ngay danh sách.
  const [running, setRunning] = useState(false);
  // Bộ lọc là lựa chọn hiển thị, giữ theo từng dự án; lọc do máy chủ làm (status, agent_id).
  const [status, setStatus] = useStored(`hq.executions.${projectId}.status`, '');
  const [agent, setAgent] = useStored(`hq.executions.${projectId}.agent`, '');
  const agents = useQuery({ queryKey: ['project', projectId, 'agents', 'for-executions'], queryFn: () => get<PageOf<Agent>>(`/projects/${projectId}/agents?limit=200`) });
  const known = agents.data?.data ?? [];
  const statusFilter = (STATUSES as readonly string[]).includes(status) ? status : '';
  // Agent đã chọn không còn trong dự án (hoặc giá trị lưu sai dạng) thì bỏ lọc.
  const agentFilter = isUuid(agent) && known.some(a => a.id === agent) ? agent : '';
  const path = `/projects/${projectId}/executions?limit=50` + (statusFilter ? '&status=' + statusFilter : '') + (agentFilter ? '&agent_id=' + agentFilter : '');
  const list = useInfiniteQuery({
    queryKey: ['project', projectId, 'executions', statusFilter, agentFilter],
    ...pages<Execution>(path),
    // Còn lần chạy chưa kết thúc thì tự làm mới.
    refetchInterval: q => (rows(q.state.data).some(x => !TERMINAL.has(x.status)) ? 5000 : false),
  });
  const filtered = statusFilter !== '' || agentFilter !== '';
  return (
    <div className="stack">
      <div className="row">
        <label className="inline">
          <span className="small muted">{t('status')}</span>
          <select value={statusFilter} onChange={e => setStatus(e.target.value)}>
            <option value="">{t('filterAllStatuses')}</option>
            {STATUSES.map(v => <option key={v} value={v}>{t(`st_${v}`)}</option>)}
          </select>
        </label>
        <label className="inline">
          <span className="small muted">{t('agent')}</span>
          <select value={agentFilter} onChange={e => setAgent(e.target.value)}>
            <option value="">{t('filterAllAgents')}</option>
            {known.map(a => <option key={a.id} value={a.id}>{a.name}</option>)}
          </select>
        </label>
        <span className="spacer" />
        <button type="button" className="primary" aria-expanded={running} onClick={() => setRunning(v => !v)}>{running ? t('close') : t('newExecution')}</button>
      </div>
      {running ? <RunForm projectId={projectId} agents={known.filter(a => a.status === 'active' && a.active_version)} /> : null}
      <Async query={list}>
        {data => (
          <>
            <DataTable
              rows={rows(data)}
              rowKey={x => x.id}
              onRow={x => navigate('/executions/' + x.id)}
              emptyText={filtered ? t('noExecutionsMatch') : t('noExecutions')}
              columns={[
                { label: t('execution'), cell: x => <Link to={'/executions/' + x.id} className="mono" onClick={e => e.stopPropagation()}>{shortId(x.id)}</Link> },
                { label: t('status'), cell: x => <StatusBadge status={x.status} /> },
                { label: t('mode'), cell: x => modeLabel(x.mode) },
                { label: t('request'), cell: x => <div className="clip" title={messageOf(x.input)}>{messageOf(x.input) || '—'}</div> },
                { label: t('cost'), className: 'num', cell: x => fmtMoney(x.estimated_cost) },
                { label: t('createdAt'), className: 'nowrap', cell: x => fmtTime(x.created_at) },
              ]}
            />
            <LoadMore query={list} />
          </>
        )}
      </Async>
    </div>
  );
}
