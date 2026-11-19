import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, Navigate, useParams } from 'react-router';
import { api, get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { KV, Notice, StatCard, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtDuration, fmtMoney, fmtNumber, fmtTime, isUuid, shortId } from '@/lib/format';
import { visible } from '@/lib/visible';
import { messageOf, modeLabel, problemText, TERMINAL } from './shared';

type Execution = Schemas['Execution'];
type Task = Schemas['Task'];
type Step = Schemas['Step'];

async function load(id: string) {
  const execution = await get<Execution>('/executions/' + id);
  const tasks = (await get<PageOf<Task>>(`/executions/${id}/tasks?limit=100`)).data;
  // Thiếu quyền đọc step của một task: task đó hiện không có bước, các task khác vẫn hiện.
  const steps = await Promise.all(tasks.map(task => soft(get<PageOf<Step>>(`/tasks/${task.id}/steps?limit=200`))));
  return { execution, tasks, steps };
}

function TaskCard({ task, steps }: { task: Task; steps: Step[] }) {
  const error = problemText(task.error);
  return (
    <div className="card">
      <div className="row">
        <strong>{task.plan_key || t('task')}</strong>
        <StatusBadge status={task.status} />
        <span className="spacer" />
        <span className="small muted">
          {`${t('iterations')}: ${task.iterations ?? 0} · ${t('toolCalls')}: ${task.tool_calls ?? 0} · ${fmtMoney(task.estimated_cost)}`}
        </span>
      </div>
      {task.goal ? <p className="small muted">{t('goal') + ': ' + task.goal}</p> : null}
      {error ? <Notice kind="bad">{error}</Notice> : null}
      {steps.length === 0 ? <div className="small muted">{t('noSteps')}</div> : (
        <ul className="steps">
          {steps.map(s => (
            <li key={s.id}>
              <span className="seq">{s.step_seq}</span>
              <span>{tOr('step_' + s.kind, s.kind)}</span>
              {s.tool_name ? <span className="mono">{s.tool_name}</span> : null}
              <StatusBadge status={s.status} />
              <span className="spacer" />
              {s.approval_id ? <Link to={'/approvals/' + s.approval_id} className="small">{t('approvalDetail')}</Link> : null}
              {s.duration_ms !== null && s.duration_ms !== undefined ? <span className="small muted">{fmtNumber(s.duration_ms) + ' ms'}</span> : null}
            </li>
          ))}
        </ul>
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
  if (!valid) return <Navigate to="/" replace />;
  return (
    <Async query={query}>
      {({ execution: x, tasks, steps }) => {
        const live = !TERMINAL.has(x.status);
        const output = messageOf(x.output);
        const error = problemText(x.error);
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
                <StatCard label={t('mode')} value={modeLabel(x.mode)} hint={t('tasks') + ': ' + tasks.length} />
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
                  {tasks.map((task, i) => <TaskCard key={task.id} task={task} steps={steps[i]?.data ?? []} />)}
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
