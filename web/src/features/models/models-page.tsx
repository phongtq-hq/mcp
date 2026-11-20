import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import { api, get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { pages, rows } from '@/api/paging';
import { DataTable } from '@/components/data-table';
import { Form, SelectField, TextField } from '@/components/form';
import { LoadMore } from '@/components/load-more';
import { Badge, ErrorBox, Notice, Page, Spinner, Success } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t, tOr } from '@/i18n';
import { capabilitiesOf, draftOf, ModelFields, type ModelDraft } from './model-fields';
import { ModelsTabs } from './models-tabs';

type Model = Schemas['Model'];
type Provider = Schemas['ModelProvider'];

// providers: undefined = đang tải; null = không đọc được; mảng = đã đọc.
function CreateModel({ providers }: { providers: Provider[] | null | undefined }) {
  const client = useQueryClient();
  const [providerId, setProviderId] = useState('');
  const [providerModel, setProviderModel] = useState('');
  const [draft, setDraft] = useState<ModelDraft>(() => draftOf());
  const [created, setCreated] = useState<string | null>(null);
  const submit = useSubmit(() => {
    const body: Schemas['ModelCreate'] = {
      provider_id: providerId, provider_model: providerModel.trim(), display_name: draft.displayName.trim(), capabilities: capabilitiesOf(draft),
    };
    if (draft.tier === 'economy' || draft.tier === 'standard' || draft.tier === 'premium') body.tier = draft.tier;
    return api<Model>('POST', '/models', body);
  }, async model => {
    setCreated(model.display_name);
    setProviderModel(''); setDraft(draftOf());
    await client.invalidateQueries({ queryKey: ['models'] });
  });
  // Model chỉ tạo được dưới provider của chính organization; provider của platform chỉ đọc.
  const own = (providers ?? []).filter(p => p.scope !== 'platform');
  return (
    <section className="card form narrow" aria-label={t('newModel')}>
      <h2>{t('newModel')}</h2>
      {providers === null ? <Notice kind="warn">{t('providersUnavailable')}</Notice> : null}
      {providers === undefined ? <p className="muted">{t('loading')}</p> : null}
      {providers && own.length === 0 ? <p className="muted">{t('newModelNeedsProvider')}</p> : null}
      <Form submit={submit} submitLabel={t('create')}>
        <SelectField label={t('provider')} required value={providerId} onChange={setProviderId} placeholder={t('choose')} options={own.map(p => ({ value: p.id, label: p.slug }))} error={submit.fields.provider_id} />
        <TextField label={t('providerModel')} required mono value={providerModel} onChange={v => { setProviderModel(v); setCreated(null); }} error={submit.fields.provider_model} hint={t('providerModelHint')} maxLength={200} />
        <ModelFields draft={draft} onChange={setDraft} errors={submit.fields} />
      </Form>
      {created && !submit.error ? <Success>{t('modelCreated', { name: created })}</Success> : null}
    </section>
  );
}

export default function ModelsPage() {
  const list = useInfiniteQuery({ queryKey: ['models', 'list'], ...pages<Model>('/models?limit=50') });
  // Tên provider cho cột của bảng và danh sách chọn ở form tạo; thiếu quyền đọc thì bảng hiện id.
  const providers = useQuery({ queryKey: ['model-providers', 'for-models'], queryFn: () => soft(get<PageOf<Provider>>('/model-providers?limit=200')) });
  const providerList = providers.isPending ? undefined : providers.data ? providers.data.data : null;
  const slugOf = new Map((providerList ?? []).map(p => [p.id, p.slug]));
  return (
    <Page title={t('navModels')} lead={t('modelsLead')}>
      <ModelsTabs current="models" />
      {list.error && !list.data ? <ErrorBox error={list.error} /> : list.isPending ? <Spinner /> : (
        <>
          <DataTable
            rows={rows(list.data)}
            rowKey={m => m.id}
            emptyText={t('noModels')}
            columns={[
              { label: t('displayName'), cell: m => <Link to={'/models/' + m.id}>{m.display_name}</Link> },
              { label: t('provider'), cell: m => <span className="mono">{slugOf.get(m.provider_id) ?? m.provider_id}</span> },
              { label: t('providerModel'), cell: m => <span className="mono">{m.provider_model}</span> },
              { label: t('tier'), cell: m => (m.tier ? tOr('tier_' + m.tier, m.tier) : '—') },
              { label: t('status'), cell: m => <Badge tone={m.enabled ? 'ok' : 'warn'}>{m.enabled ? t('enabled') : t('disabled')}</Badge> },
              { label: t('pricing'), cell: m => <Badge tone={m.has_current_pricing ? 'ok' : 'bad'}>{m.has_current_pricing ? t('pricingSet') : t('pricingMissing')}</Badge> },
            ]}
          />
          <LoadMore query={list} />
        </>
      )}
      <CreateModel providers={providerList} />
    </Page>
  );
}
