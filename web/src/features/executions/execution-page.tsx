import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, Navigate, useParams } from 'react-router';
import { api, get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { ErrorBox, KV, Notice, Spinner, StatCard, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtDuration, fmtMoney, fmtNumber, fmtTime, isUuid, shortId } from '@/lib/format';
import { useTitle } from '@/lib/use-title';
import { json, visible } from '@/lib/visible';
import { messageOf, modeLabel, problemText, TERMINAL } from './shared';

type Execution = Schemas['Execution'];
type Task = Schemas['Task'];
type Step = Schemas['Step'];
type Agent = Schemas['Agent'];

async function load(id: string) {
  const execution = await get<Execution>('/executions/' + id);
  const tasks = (await get<PageOf<Task>>(`/executions/${id}/tasks?limit=100`)).data;
  // Thiếu quyền đọc step của một task: task đó hiện không có bước, các task khác vẫn hiện.
  const steps = await Promise.all(tasks.map(task => soft(get<PageOf<Step>>(`/tasks/${task.id}/steps?limit=200`))));
  // Tên Agent để người đọc không phải nhận ra Agent qua mã. Thiếu quyền đọc Agent thì hiện mã.
  const agentIds = [...new Set(tasks.map(task => task.agent_id))];
  const found = await Promise.all(agentIds.map(agentId => soft(get<Agent>('/agents/' + agentId))));
  const agents = new Map<string, Agent>();
  found.forEach(a => { if (a) agents.set(a.id, a); });
  return { execution, tasks, steps, agents };
}

function agentLabel(task: Task, agents: Map<string, Agent>): string {
  return (agents.get(task.agent_id)?.name ?? shortId(task.agent_id)) + ' · v' + task.agent_version;
}

// content: nội dung request / response của một step để hiển thị. API trả nó dạng chuỗi JSON
// (openapi.yaml ghi là object): nhận cả hai. Nội dung do model và tool sinh ra, chỉ hiện dạng
// văn bản và qua json() để lộ ký tự ẩn.
function content(value: unknown): string | null {
  if (value === null || value === undefined || value === '') return null;
  if (typeof value === 'string') {
    try { return json(JSON.parse(value)); } catch { return visible(value); }
  }
  return json(value);
}

// modelUsed: tên model đã trả lời lượt này, đọc từ `routing` của step (cùng lưu ý về dạng).
function modelUsed(step: Step): string | null {
  let routing: unknown = step.routing;
  if (typeof routing === 'string') {
    try { routing = JSON.parse(routing); } catch { return null; }
  }
  if (!routing || typeof routing !== 'object') return null;
  const name = (routing as Record<string, unknown>).model_used;
  return typeof name === 'string' && name ? name : null;
}

function StepContent({ step }: { step: Step }) {
  const parts: [string, string | null][] = [[t('stepRequest'), content(step.request)], [t('stepResponse'), content(step.response)], [t('error'), content(step.error)]];
  const shown = parts.filter((x): x is [string, string] => x[1] !== null);
  if (shown.length === 0) return <div className="small muted">{t('stepNoContent')}</div>;
  return <>{shown.map(([label, text]) => <div key={label}><div className="small muted">{label}</div><pre>{text}</pre></div>)}</>;
}

function TaskCard({ task, steps, agents }: { task: Task; steps: Step[]; agents: Map<string, Agent> }) {
  const error = problemText(task.error);
  // Nội dung của các bước chỉ tải khi người dùng bấm xem: nó cần quyền riêng
  // (execution:read_content) và có thể lớn.
  const [open, setOpen] = useState(false);
  const detail = useQuery({
    queryKey: ['task', task.id, 'steps', 'content'],
    queryFn: () => get<PageOf<Step>>(`/tasks/${task.id}/steps?limit=200&include_content=true`),
    enabled: open,
  });
  const full = new Map((detail.data?.data ?? []).map(s => [s.id, s]));
  return (
    <div className="card">
      <div className="row">
        <strong>{task.plan_key || t('task')}</strong>
        <span className="small muted">{agentLabel(task, agents)}</span>
        <StatusBadge status={task.status} />
        <span className="spacer" />
        <span className="small muted">
          {`${t('iterations')}: ${task.iterations ?? 0} · ${t('toolCalls')}: ${task.tool_calls ?? 0} · ${fmtMoney(task.estimated_cost)}`}
        </span>
      </div>
      {task.goal ? <p className="small muted">{t('goal') + ': ' + task.goal}</p> : null}
      {error ? <Notice kind="bad">{error}</Notice> : null}
      {steps.length === 0 ? <div className="small muted">{t('noSteps')}</div> : (
        <>
          <ul className="steps">
            {steps.map(s => {
              const loaded = full.get(s.id);
              return (
                <li key={s.id}>
                  <span className="seq">{s.step_seq}</span>
                  <span>{tOr('step_' + s.kind, s.kind)}</span>
                  {s.tool_name ? <span className="mono">{s.tool_name}</span> : null}
                  <StatusBadge status={s.status} />
                  <span className="spacer" />
                  {s.approval_id ? <Link to={'/approvals/' + s.approval_id} className="small">{t('approvalDetail')}</Link> : null}
                  {s.duration_ms !== null && s.duration_ms !== undefined ? <span className="small muted">{fmtNumber(s.duration_ms) + ' ms'}</span> : null}
                  {open && loaded ? <div className="step-content reveal"><StepContent step={loaded} /></div> : null}
                </li>
              );
            })}
          </ul>
          <div className="row">
            <button type="button" aria-expanded={open} onClick={() => setOpen(v => !v)}>{open ? t('stepHideContent') : t('stepShowContent')}</button>
            {open && detail.isPending ? <Spinner /> : null}
          </div>
          {open && detail.error ? <ErrorBox error={detail.error} /> : null}
        </>
      )}
    </div>
  );
}

function CancelButton({ id }: { id: string }) {
  const client = useQueryClient();
  const [busy, setBusy] = useState(false);
  const cancel = async () => {
    if (!window.confirm(t('cancelConfirm'))) return;
    setBusy(true);
    try {
      await api<unknown>('POST', `/executions/${id}/cancel`);
      await client.invalidateQueries({ queryKey: ['execution', id] });
    } catch (err) {
      window.alert(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  };
  return <button className="danger" disabled={busy} onClick={() => { void cancel(); }}>{t('cancelExecution')}</button>;
}

export default function ExecutionPage() {
  const { executionId } = useParams();
  const valid = isUuid(executionId);
  const id = valid ? executionId : '';
  const query = useQuery({
    queryKey: ['execution', id],
    queryFn: () => load(id),
    enabled: valid,
    refetchInterval: q => (q.state.data && !TERMINAL.has(q.state.data.execution.status) ? 3000 : false),
  });
  useTitle(valid ? t('execution') + ' ' + shortId(id) : null);
  if (!valid) return <Navigate to="/" replace />;
  return (
    <Async loading="detail" query={query} back={<Link className="btn" to="/projects">{t('navProjects')}</Link>}>
      {({ execution: x, tasks, steps, agents }) => {
        const live = !TERMINAL.has(x.status);
        const output = messageOf(x.output);
        const error = problemText(x.error);
        // Một task: nêu Agent và model ngay ở đầu trang. Nhiều task (kế hoạch): từng thẻ task tự nêu.
        const only = tasks.length === 1 ? tasks[0] : undefined;
        const model = only ? (steps[0]?.data ?? []).map(modelUsed).find(Boolean) : null;
        return (
          <>
            <div className="crumbs">
              <Link to="/projects">{t('navProjects')}</Link> / <Link to={'/projects/' + x.project_id}>{t('tabExecutions')}</Link> / <span className="mono">{shortId(x.id)}</span>
            </div>
            <div className="page-head">
              <div>
                <h1>{t('execution')} <span className="mono">{shortId(x.id)}</span> <StatusBadge status={x.status} /></h1>
                {live ? <p className="sub">{t('autoRefreshing')}</p> : null}
              </div>
              <div className="row">{live ? <CancelButton id={x.id} /> : null}</div>
            </div>
            <div className="stack">
              {x.status === 'waiting_approval' ? <Notice kind="warn">{t('waitingApprovalNote')} <Link to="/approvals">{t('goToApprovals')}</Link></Notice> : null}
              {error ? <Notice kind="bad"><strong>{t('error')}: </strong>{error}</Notice> : null}
              <div className="grid">
                <StatCard
                  label={t('costEstimated')} value={fmtMoney(x.estimated_cost)}
                  hint={x.budget && x.budget.amount ? t('budget') + ': ' + fmtMoney(x.budget) : null}
                />
                <StatCard label={t('duration')} value={fmtDuration(x.started_at || x.created_at, x.finished_at)} hint={fmtTime(x.started_at || x.created_at)} />
                {only
                  ? <StatCard label={t('agent')} value={agents.get(only.agent_id)?.name ?? shortId(only.agent_id)} hint={'v' + only.agent_version + (model ? ' · ' + model : '')} />
                  : <StatCard label={t('mode')} value={modeLabel(x.mode)} hint={t('tasks') + ': ' + tasks.length} />}
              </div>
              <div className="card">
                <h2>{t('input')}</h2>
                <div className="answer">{visible(messageOf(x.input)) || '—'}</div>
              </div>
              <div className="card">
                <h2>{t('answer')}</h2>
                {output
                  ? <><div className="small muted">{t('modelGenerated')}</div><div className="answer">{visible(output)}</div></>
                  : <div className="muted">{t('noOutputYet')}</div>}
              </div>
              <div>
                <h2>{t('tasks')}</h2>
                <div className="stack">
                  {tasks.map((task, i) => <TaskCard key={task.id} task={task} steps={steps[i]?.data ?? []} agents={agents} />)}
                </div>
              </div>
              <div className="card">
                <KV pairs={[
                  [t('initiator'), x.initiator ? `${tOr('actor_' + x.initiator.type, x.initiator.type || '')} ${shortId(x.initiator.id)}` : '—'],
                  [t('createdAt'), fmtTime(x.created_at)],
                  [t('finishedAt'), fmtTime(x.finished_at)],
                  [t('traceId'), <span className="mono">{x.trace_id || '—'}</span>],
                  ['ID', <span className="mono">{x.id}</span>],
                ]} />
              </div>
            </div>
          </>
        );
      }}
    </Async>
  );
}
