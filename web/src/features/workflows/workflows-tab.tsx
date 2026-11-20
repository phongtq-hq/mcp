import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useParams } from 'react-router';
import { api, get, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { ErrorBox, Notice, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtDuration, fmtTime } from '@/lib/format';

type Workflow = Schemas['Workflow'];
type WorkflowExecution = Schemas['WorkflowExecution'];

function stepCount(w: Workflow): number | string {
  const steps: unknown = (w.definition as { steps?: unknown } | undefined)?.steps;
  return Array.isArray(steps) ? steps.length : '—';
}

function Runs({ workflow }: { workflow: Workflow }) {
  const query = useQuery({
    queryKey: ['workflow', workflow.id, 'executions'],
    queryFn: () => get<PageOf<WorkflowExecution>>(`/workflows/${workflow.id}/executions?limit=20`),
  });
  return (
    <div>
      <Async query={query}>
        {list => (
          <>
            <h2>{`${t('workflowRuns')}: ${workflow.name}`}</h2>
            <DataTable
              rows={list.data}
              rowKey={x => x.id}
              emptyText={t('noRuns')}
              columns={[
                { label: t('status'), cell: x => <StatusBadge status={x.status} /> },
                { label: t('trigger'), cell: x => tOr('trig_' + x.trigger_type, x.trigger_type) },
                { label: t('version'), className: 'num', cell: x => x.workflow_version },
                { label: t('startedAt'), className: 'nowrap', cell: x => fmtTime(x.started_at || x.created_at) },
                { label: t('duration'), className: 'nowrap', cell: x => fmtDuration(x.started_at || x.created_at, x.finished_at) },
                { label: t('execution'), cell: x => x.execution_id ? <Link to={'/executions/' + x.execution_id}>{t('details')}</Link> : '—' },
              ]}
            />
          </>
        )}
      </Async>
    </div>
  );
}

// Tab workflow của một dự án. projectId đã được khung dự án kiểm dạng.
export default function WorkflowsTab() {
  const { projectId } = useParams();
  const client = useQueryClient();
  const [selected, setSelected] = useState<Workflow | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [status, setStatus] = useState<{ ok: true } | { ok: false; error: unknown } | null>(null);
  const query = useQuery({
    queryKey: ['project', projectId, 'workflows'],
    queryFn: () => get<PageOf<Workflow>>(`/projects/${projectId}/workflows?limit=200`),
  });

  const run = async (w: Workflow) => {
    if (!window.confirm(t('runWorkflowConfirm', { name: w.name }))) return;
    setBusy(w.id);
    setStatus(null);
    try {
      await api<unknown>('POST', `/workflows/${w.id}/executions`, { input: {} });
      setStatus({ ok: true });
      setSelected(w);
      await client.invalidateQueries({ queryKey: ['workflow', w.id, 'executions'] });
    } catch (error) {
      setStatus({ ok: false, error });
    }
    setBusy(null);
  };

  return (
    <>
      {status ? (status.ok ? <Notice kind="ok">{t('workflowStarted')}</Notice> : <ErrorBox error={status.error} />) : null}
      <Async query={query}>
        {r => (
          <DataTable
            rows={r.data}
            rowKey={w => w.id}
            onRow={setSelected}
            emptyText={t('noWorkflows')}
            emptyHint={t('noWorkflowsHint')}
            columns={[
              { label: t('name'), cell: w => <><div>{w.name}</div>{w.description ? <div className="small muted">{w.description}</div> : null}</> },
              { label: t('status'), cell: w => <StatusBadge status={w.status} /> },
              { label: t('schedule'), cell: w => w.schedule ? <span className="mono">{w.schedule + ' (UTC)'}</span> : <span className="muted">{t('noSchedule')}</span> },
              { label: t('stepsCount'), className: 'num', cell: stepCount },
              { label: t('version'), className: 'num', cell: w => w.version },
              {
                label: '',
                cell: w => (
                  <button
                    disabled={w.status !== 'active' || busy === w.id}
                    title={w.status !== 'active' ? t('workflowNotActive') : undefined}
                    onClick={e => { e.stopPropagation(); void run(w); }}
                  >
                    {t('runWorkflow')}
                  </button>
                ),
              },
            ]}
          />
        )}
      </Async>
      {selected ? <Runs key={selected.id} workflow={selected} /> : null}
    </>
  );
}
