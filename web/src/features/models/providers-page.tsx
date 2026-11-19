import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import { api, type Schemas } from '@/api/client';
import { pages, rows } from '@/api/paging';
import { DataTable } from '@/components/data-table';
import { Form, TextField } from '@/components/form';
import { LoadMore } from '@/components/load-more';
import { Badge, ErrorBox, Notice, Page, Spinner } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t, tOr } from '@/i18n';
import { ApiError } from '@/lib/api-error';
import { ModelsTabs } from './models-tabs';

type Provider = Schemas['ModelProvider'];

function CreateProvider() {
  const client = useQueryClient();
  const [slug, setSlug] = useState('');
  const [adapter, setAdapter] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [created, setCreated] = useState<string | null>(null);
  const submit = useSubmit(async () => {
    const body: Schemas['ModelProviderCreate'] = { slug: slug.trim(), adapter: adapter.trim() };
    // Khoá chỉ đi một chiều: gửi lên, máy chủ kiểm với provider rồi mã hoá; không bao giờ đọc lại được.
    if (apiKey !== '') body.api_key = apiKey;
    try {
      return await api<Provider>('POST', '/model-providers', body);
    } catch (err) {
      // Máy chủ đã trả lời từ chối (4xx): khoá không còn lý do nằm lại trong ô. Chỉ giữ khi
      // kết cục không rõ (mất mạng, 5xx), vì lần gửi lại phải cùng thân để dùng lại Idempotency-Key.
      if (err instanceof ApiError && err.status >= 400 && err.status < 500) setApiKey('');
      throw err;
    }
  }, async provider => {
    setCreated(provider.slug);
    setSlug(''); setAdapter(''); setApiKey('');
    await client.invalidateQueries({ queryKey: ['model-providers'] });
  });
  return (
    <section className="card form narrow" aria-label={t('newProvider')}>
      <h2>{t('newProvider')}</h2>
      <Form submit={submit} submitLabel={t('create')}>
        <TextField label={t('slug')} required mono value={slug} onChange={v => { setSlug(v); setCreated(null); }} error={submit.fields.slug} hint={t('slugHint')} maxLength={63} />
        <TextField label={t('adapter')} required mono value={adapter} onChange={setAdapter} error={submit.fields.adapter} hint={t('adapterHint')} />
        <TextField label={t('apiKey')} type="password" autoComplete="new-password" value={apiKey} onChange={setApiKey} error={submit.fields.api_key} hint={t('apiKeyHint')} />
      </Form>
      {created && !submit.error ? <Notice kind="ok">{t('providerCreated', { slug: created })}</Notice> : null}
    </section>
  );
}

export default function ProvidersPage() {
  const list = useInfiniteQuery({ queryKey: ['model-providers', 'list'], ...pages<Provider>('/model-providers?limit=50') });
  return (
    <Page title={t('navModels')} lead={t('providersLead')}>
      <ModelsTabs current="providers" />
      {list.error && !list.data ? <ErrorBox error={list.error} /> : list.isPending ? <Spinner /> : (
        <>
          <DataTable
            rows={rows(list.data)}
            rowKey={p => p.id}
            emptyText={t('noProviders')}
            columns={[
              { label: t('slug'), cell: p => <Link className="mono" to={'/models/providers/' + p.id}>{p.slug}</Link> },
              { label: t('adapter'), cell: p => <span className="mono">{p.adapter}</span> },
              { label: t('scope'), cell: p => <Badge tone={p.scope === 'platform' ? 'info' : undefined}>{tOr('scope_' + (p.scope ?? ''), p.scope ?? '—')}</Badge> },
              { label: t('status'), cell: p => <Badge tone={p.enabled ? 'ok' : 'warn'}>{p.enabled ? t('enabled') : t('disabled')}</Badge> },
              { label: t('credential'), cell: p => (p.has_credential ? t('credentialSet') : t('credentialNone')) },
            ]}
          />
          <LoadMore query={list} />
        </>
      )}
      <CreateProvider />
    </Page>
  );
}
