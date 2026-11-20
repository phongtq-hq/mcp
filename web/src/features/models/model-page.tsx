import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, Navigate, useParams } from 'react-router';
import { api, getWithETag, type Schemas } from '@/api/client';
import { pages, rows } from '@/api/paging';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { CheckField, Form, TextField } from '@/components/form';
import { LoadMore } from '@/components/load-more';
import { Section, useSections } from '@/components/sections';
import { Badge, ErrorBox, KV, Notice, Page, Spinner } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t } from '@/i18n';
import { fmtRate, fmtTime, isUuid, localInputToISO } from '@/lib/format';
import { capabilitiesOf, draftOf, ModelFields, type ModelDraft } from './model-fields';

type Model = Schemas['Model'];
type Pricing = Schemas['ModelPricing'];

function EditForm({ model, patch, onChange, onSaved }: { model: Model; patch: (body: Schemas['ModelUpdate']) => Promise<Model>; onChange: () => void; onSaved: () => Promise<void> }) {
  const [draft, setDraft] = useState<ModelDraft>(() => draftOf(model));
  const [enabled, setEnabled] = useState(model.enabled);
  const submit = useSubmit(() => {
    const body: Schemas['ModelUpdate'] = { display_name: draft.displayName.trim(), enabled, capabilities: capabilitiesOf(draft, model.capabilities) };
    // Máy chủ giữ tier cũ khi không nhận tier (không gỡ được), nên chỉ gửi khi có chọn.
    if (draft.tier === 'economy' || draft.tier === 'standard' || draft.tier === 'premium') body.tier = draft.tier;
    return patch(body);
  }, onSaved);
  return (
    <Form submit={submit}>
      <ModelFields draft={draft} onChange={d => { setDraft(d); onChange(); }} errors={submit.fields} tierLocked={!!model.tier} />
      <CheckField label={t('modelEnabled')} hint={t('modelEnabledHint')} checked={enabled} onChange={v => { setEnabled(v); onChange(); }} />
    </Form>
  );
}

const AMOUNTS = [
  ['input_per_mtok', 'priceInput', true],
  ['output_per_mtok', 'priceOutput', true],
  ['cached_input_per_mtok', 'priceCachedInput', false],
  ['cache_write_per_mtok', 'priceCacheWrite', false],
  ['reasoning_per_mtok', 'priceReasoning', false],
] as const;
type AmountField = typeof AMOUNTS[number][0];

// Thêm một phiên bản giá. Giá là bất biến: không sửa, không xoá; giá đang hiệu lực được
// đóng tại effective_from của giá mới. Số tiền là chuỗi thập phân, gửi nguyên văn (không
// qua số thực của trình duyệt).
function PricingForm({ modelId, onSaved }: { modelId: string; onSaved: () => Promise<void> }) {
  const empty: Record<AmountField, string> = { input_per_mtok: '', output_per_mtok: '', cached_input_per_mtok: '', cache_write_per_mtok: '', reasoning_per_mtok: '' };
  const [currency, setCurrency] = useState('USD');
  const [amounts, setAmounts] = useState(empty);
  const [now, setNow] = useState(true);
  const [from, setFrom] = useState('');
  const [source, setSource] = useState('');
  const [done, setDone] = useState(false);
  const submit = useSubmit(() => {
    const body: Schemas['ModelPricingCreate'] = { currency: currency.trim(), input_per_mtok: amounts.input_per_mtok.trim(), output_per_mtok: amounts.output_per_mtok.trim() };
    // "Áp dụng ngay": không gửi effective_from, máy chủ dùng giờ của nó. Giờ của trình duyệt
    // có thể lệch, và một thời điểm tính ở đây làm thân của lần gửi lại khác lần đầu (mất
    // tác dụng của Idempotency-Key).
    if (!now) {
      const effective = localInputToISO(from);
      if (!effective) throw new Error(t('priceFromRequired'));
      body.effective_from = effective;
    }
    for (const key of ['cached_input_per_mtok', 'cache_write_per_mtok', 'reasoning_per_mtok'] as const) {
      if (amounts[key].trim() !== '') body[key] = amounts[key].trim();
    }
    if (source.trim() !== '') body.source = source.trim();
    return api<Pricing>('POST', `/models/${modelId}/pricing`, body);
  }, async () => { setDone(true); setAmounts(empty); setSource(''); await onSaved(); });
  return (
    <Form submit={submit} submitLabel={t('addPrice')}>
      <p className="muted">{t('priceImmutable')}</p>
      <TextField label={t('currency')} required mono value={currency} onChange={setCurrency} error={submit.fields.currency} maxLength={3} />
      <div className="grid">
        {AMOUNTS.map(([field, label, required]) => (
          <TextField key={field} label={t(label)} required={required} mono value={amounts[field]} onChange={v => { setAmounts(a => ({ ...a, [field]: v })); setDone(false); }} error={submit.fields[field]} />
        ))}
      </div>
      <CheckField label={t('priceFromNow')} checked={now} onChange={setNow} />
      {now ? null : <TextField label={t('priceFrom')} type="datetime-local" required value={from} onChange={setFrom} error={submit.fields.effective_from} hint={t('priceFromHint')} />}
      <TextField label={t('priceSource')} value={source} onChange={setSource} error={submit.fields.source} hint={t('priceSourceHint')} maxLength={500} />
      {done && !submit.error ? <Notice kind="ok">{t('saved')}</Notice> : null}
    </Form>
  );
}

function PricingSection({ modelId }: { modelId: string }) {
  const client = useQueryClient();
  const list = useInfiniteQuery({ queryKey: ['model', modelId, 'pricing'], ...pages<Pricing>(`/models/${modelId}/pricing?limit=50`) });
  const reload = async () => {
    await Promise.all([client.invalidateQueries({ queryKey: ['model', modelId] }), client.invalidateQueries({ queryKey: ['models'] })]);
  };
  const amount = fmtRate;
  return (
    <section className="card form" aria-label={t('pricing')}>
      <h2>{t('pricing')}</h2>
      {list.error && !list.data ? <ErrorBox error={list.error} /> : list.isPending ? <Spinner /> : (
        <>
          <DataTable
            rows={rows(list.data)}
            rowKey={p => p.id}
            emptyText={t('noPricing')}
            columns={[
              { label: t('version'), className: 'num', cell: p => p.version },
              { label: t('scope'), cell: p => (p.scope === 'organization' ? t('priceScopeOrg') : t('priceScopeList')) },
              { label: t('currency'), cell: p => p.currency },
              { label: t('priceInput'), className: 'num', cell: p => amount(p.input_per_mtok) },
              { label: t('priceOutput'), className: 'num', cell: p => amount(p.output_per_mtok) },
              { label: t('priceCachedInput'), className: 'num', cell: p => amount(p.cached_input_per_mtok) },
              { label: t('priceCacheWrite'), className: 'num', cell: p => amount(p.cache_write_per_mtok) },
              { label: t('priceReasoning'), className: 'num', cell: p => amount(p.reasoning_per_mtok) },
              { label: t('priceFrom'), className: 'nowrap', cell: p => fmtTime(p.effective_from) },
              { label: t('priceTo'), className: 'nowrap', cell: p => (p.effective_to ? fmtTime(p.effective_to) : t('priceOpen')) },
              { label: t('priceSource'), cell: p => p.source ?? '—' },
            ]}
          />
          <LoadMore query={list} />
        </>
      )}
      <PricingForm modelId={modelId} onSaved={reload} />
    </section>
  );
}

export default function ModelPage() {
  const { modelId } = useParams();
  const client = useQueryClient();
  const sections = useSections();
  const valid = isUuid(modelId);
  const query = useQuery({ queryKey: ['model', modelId, 'detail'], queryFn: () => getWithETag<Model>('/models/' + modelId), enabled: valid });
  if (!valid) return <Navigate to="/models" replace />;
  return (
    <>
      <div className="crumbs"><Link to="/models">{t('tabModels')}</Link></div>
      <Async query={query}>
        {loaded => {
          const model = loaded.data;
          const etag = loaded.etag;
          const patch = (body: Schemas['ModelUpdate']) => {
            if (!etag) return Promise.reject(new Error(t('errNoETag')));
            return api<Model>('PATCH', '/models/' + model.id, body, { ifMatch: etag });
          };
          const saved = async () => {
            sections.markSaved('edit');
            await Promise.all([client.invalidateQueries({ queryKey: ['model', modelId, 'detail'] }), client.invalidateQueries({ queryKey: ['models'] })]);
          };
          return (
            <Page title={model.display_name}>
              <section className="card">
                <KV pairs={[
                  [t('providerModel'), <span className="mono">{model.provider_model}</span>],
                  [t('status'), <Badge tone={model.enabled ? 'ok' : 'warn'}>{model.enabled ? t('enabled') : t('disabled')}</Badge>],
                  [t('pricing'), <Badge tone={model.has_current_pricing ? 'ok' : 'bad'}>{model.has_current_pricing ? t('pricingSet') : t('pricingMissing')}</Badge>],
                  model.deprecated_at ? [t('deprecatedAt'), fmtTime(model.deprecated_at)] : null,
                  [t('id'), <span className="mono">{model.id}</span>],
                ]} />
              </section>
              {model.has_current_pricing ? null : <Notice kind="warn">{t('pricingMissingHint')}</Notice>}
              <Section name="edit" title={t('edit')} sections={sections} narrow>
                <EditForm key={(etag ?? '') + ':' + sections.nonce} model={model} patch={patch} onChange={() => sections.touch('edit')} onSaved={saved} />
              </Section>
              <PricingSection modelId={model.id} />
            </Page>
          );
        }}
      </Async>
    </>
  );
}
