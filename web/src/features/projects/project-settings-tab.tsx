import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import { api, get, getWithETag, soft, type Page as PageOf, type Schemas, type WithETag } from '@/api/client';
import { Async } from '@/components/async';
import { useConfirm } from '@/components/confirm';
import { CheckField, Form, SelectField, SubmitError, TextField } from '@/components/form';
import { RoutingForm } from '@/components/routing-form';
import { Section, useSections } from '@/components/sections';
import { SettingsForm } from '@/components/settings-form';
import { useSubmit } from '@/components/use-submit';
import { t } from '@/i18n';

type Project = Schemas['Project'];
type Agent = Schemas['Agent'];
type Model = Schemas['Model'];

function asObject(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

interface Loaded { project: WithETag<Project>; agents: Agent[] | null; models: { value: string; label: string }[] | null; modelsTruncated: boolean }

interface FormProps {
  loaded: Loaded;
  // patch gửi PATCH kèm ETag của bản đang hiện; mọi mục dùng chung nó.
  patch: (body: Schemas['ProjectUpdate']) => Promise<Project>;
  onChange: () => void;
  onSaved: () => Promise<void>;
}

function NameForm({ loaded, patch, onChange, onSaved }: FormProps) {
  const [name, setName] = useState(loaded.project.data.name);
  const submit = useSubmit(() => patch({ name: name.trim() }), onSaved);
  return (
    <Form submit={submit}>
      <TextField label={t('name')} required value={name} onChange={v => { setName(v); onChange(); }} error={submit.fields.name} maxLength={200} />
    </Form>
  );
}

// Mục điều phối: khoá `orchestration` của settings. Mỗi lần ghi thay cả settings, nên các
// khoá khác (model_routing…) được gửi lại nguyên vẹn từ bản vừa đọc; trong `orchestration`
// cũng vậy: khoá mà form này không biết được giữ nguyên.
function OrchestrationForm({ loaded, patch, onChange, onSaved }: FormProps) {
  const settings = asObject(loaded.project.data.settings);
  const current = asObject(settings.orchestration);
  const [enabled, setEnabled] = useState(current.planned_enabled === true);
  const [planner, setPlanner] = useState(typeof current.planner_agent_id === 'string' ? current.planner_agent_id : '');
  const [parallel, setParallel] = useState(typeof current.max_parallel_tasks === 'number' ? String(current.max_parallel_tasks) : '');

  const submit = useSubmit(() => {
    const orchestration: Record<string, unknown> = { ...current, planned_enabled: enabled };
    if (planner) orchestration.planner_agent_id = planner; else delete orchestration.planner_agent_id;
    if (parallel.trim() !== '') orchestration.max_parallel_tasks = Number(parallel); else delete orchestration.max_parallel_tasks;
    return patch({ settings: { ...settings, orchestration } });
  }, onSaved);

  const agents = loaded.agents ?? [];
  // Agent đã chọn mà không còn trong danh sách (đã lưu trữ, hoặc thiếu quyền đọc) vẫn hiện
  // bằng id, để lưu lại không lặng lẽ bỏ nó.
  const options = agents.map(a => ({ value: a.id, label: a.name }));
  if (planner && !options.some(o => o.value === planner)) options.unshift({ value: planner, label: planner });

  return (
    <Form submit={submit}>
      <CheckField label={t('plannedEnabled')} hint={t('plannedEnabledHint')} checked={enabled} onChange={v => { setEnabled(v); onChange(); }} />
      <SelectField
        label={t('plannerAgent')}
        hint={loaded.agents === null ? t('plannerAgentNoList') : t('plannerAgentHint')}
        value={planner}
        onChange={v => { setPlanner(v); onChange(); }}
        placeholder={t('none')}
        options={options}
      />
      <TextField
        label={t('maxParallelTasks')}
        hint={t('maxParallelTasksHint')}
        type="number"
        value={parallel}
        onChange={v => { setParallel(v); onChange(); }}
      />
    </Form>
  );
}

function ArchiveSection({ project }: { project: Project }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const [dialog, confirm] = useConfirm();
  const submit = useSubmit(async () => {
    const ok = await confirm({ title: t('archiveProjectConfirm', { name: project.name }), body: t('archiveProjectBody'), confirmLabel: t('archive'), danger: true });
    if (!ok) return false;
    await api<null>('DELETE', '/projects/' + project.id);
    return true;
  }, async done => {
    if (!done) return;
    await Promise.all([client.invalidateQueries({ queryKey: ['projects'] }), client.invalidateQueries({ queryKey: ['overview'] })]);
    void navigate('/projects');
  });
  return (
    <section className="card form narrow" aria-label={t('dangerZone')}>
      <h2>{t('dangerZone')}</h2>
      <p className="muted">{t('archiveProjectBody')}</p>
      <SubmitError submit={submit} />
      <div className="row">
        <button type="button" className="danger" disabled={submit.busy} onClick={() => { void submit.run(); }}>{t('archiveProject')}</button>
      </div>
      {dialog}
    </section>
  );
}

// Tab Cài đặt của một dự án. projectId đã được khung dự án kiểm dạng.
export default function ProjectSettingsTab() {
  const { projectId } = useParams();
  const client = useQueryClient();
  const sections = useSections();
  const query = useQuery({
    queryKey: ['project', projectId, 'settings'],
    queryFn: async (): Promise<Loaded> => {
      const [project, agents, models] = await Promise.all([
        getWithETag<Project>('/projects/' + projectId),
        soft(get<PageOf<Agent>>(`/projects/${projectId}/agents?limit=200`)),
        soft(get<PageOf<Model>>('/models?enabled=true&limit=200')),
      ]);
      return { project, agents: agents ? agents.data : null, models: models ? models.data.map(m => ({ value: m.id, label: m.display_name })) : null, modelsTruncated: !!models?.next_cursor };
    },
  });
  return (
    <Async loading="detail" query={query}>
      {loaded => {
        const etag = loaded.project.etag;
        // Không có ETag thì không sửa: gửi PATCH thiếu If-Match là ghi vô điều kiện, và với
        // settings đó là thay cả object bằng bản có thể đã cũ.
        const patch = (body: Schemas['ProjectUpdate']) => {
          if (!etag) return Promise.reject(new Error(t('errNoETag')));
          return api<Project>('PATCH', '/projects/' + loaded.project.data.id, body, { ifMatch: etag });
        };
        // Lưu xong: đọc lại dự án (tiêu đề của khung dự án, danh sách, tổng quan), rồi mọi
        // form dựng lại từ bản mới với ETag mới (key bên dưới).
        const saved = (name: string) => async () => {
          sections.markSaved(name);
          await Promise.all([
            client.invalidateQueries({ queryKey: ['project', projectId] }),
            client.invalidateQueries({ queryKey: ['projects'] }),
            client.invalidateQueries({ queryKey: ['overview'] }),
          ]);
        };
        const version = (etag ?? '') + ':' + sections.nonce;
        const props = (name: string): FormProps => ({ loaded, patch, onChange: () => sections.touch(name), onSaved: saved(name) });
        return (
          <>
            <Section name="name" title={t('generalInfo')} sections={sections} narrow>
              <NameForm key={version} {...props('name')} />
            </Section>
            <Section name="orchestration" title={t('orchestration')} sections={sections} narrow>
              <OrchestrationForm key={version} {...props('orchestration')} />
            </Section>
            <Section name="routing" title={t('projectRouting')} sections={sections}>
              <p className="muted">{t('projectRoutingHint')}</p>
              <RoutingForm
                key={version}
                settings={loaded.project.data.settings}
                models={loaded.models}
                truncated={loaded.modelsTruncated}
                save={settings => patch({ settings })}
                onChange={() => sections.touch('routing')}
                onSaved={saved('routing')}
              />
            </Section>
            <Section name="settings" title={t('advancedSettings')} sections={sections}>
              <SettingsForm
                key={version}
                settings={loaded.project.data.settings}
                save={settings => patch({ settings })}
                onChange={() => sections.touch('settings')}
                onSaved={saved('settings')}
              />
            </Section>
            <ArchiveSection project={loaded.project.data} />
          </>
        );
      }}
    </Async>
  );
}
