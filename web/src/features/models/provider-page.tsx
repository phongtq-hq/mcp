import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, Navigate, useNavigate, useParams } from 'react-router';
import { api, getWithETag, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { useConfirm } from '@/components/confirm';
import { CheckField, Form, SubmitError, TextField } from '@/components/form';
import { Section, useSections } from '@/components/sections';
import { SettingsForm } from '@/components/settings-form';
import { Badge, KV, Notice, Page } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t, tOr } from '@/i18n';
import { isUuid } from '@/lib/format';

type Provider = Schemas['ModelProvider'];
type Update = Schemas['ModelProviderUpdate'];

interface FormProps { provider: Provider; patch: (body: Update) => Promise<Provider>; onChange: () => void; onSaved: () => Promise<void> }

function EnabledForm({ provider, patch, onChange, onSaved }: FormProps) {
  const [enabled, setEnabled] = useState(provider.enabled);
  const submit = useSubmit(() => patch({ enabled }), onSaved);
  return (
    <Form submit={submit}>
      <CheckField label={t('providerEnabled')} hint={t('providerEnabledHint')} checked={enabled} onChange={v => { setEnabled(v); onChange(); }} />
    </Form>
  );
}

// Xoay khoá: gửi khoá mới; máy chủ kiểm với provider trước khi lưu (sai → 422, không kiểm
// được → 503; cả hai đều không lưu gì). Ô nhập bị xoá ngay khi gửi xong, dù kết quả thế nào
// khoá cũng không nằm lại trên màn hình lâu hơn cần thiết.
function CredentialForm({ provider, patch, onChange, onSaved }: FormProps) {
  const [apiKey, setApiKey] = useState('');
  const submit = useSubmit(async () => {
    try { return await patch({ api_key: apiKey }); } finally { setApiKey(''); }
  }, onSaved);
  return (
    <Form submit={submit} submitLabel={provider.has_credential ? t('rotateKey') : t('setKey')} disabled={apiKey === ''}>
      <p className="muted">{provider.has_credential ? t('credentialSet') : t('credentialNone')}</p>
      <TextField label={t('apiKeyNew')} type="password" autoComplete="new-password" value={apiKey} onChange={v => { setApiKey(v); onChange(); }} error={submit.fields.api_key} hint={t('apiKeyHint')} />
    </Form>
  );
}

function DeleteSection({ provider }: { provider: Provider }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const [dialog, confirm] = useConfirm();
  const submit = useSubmit(async () => {
    if (!await confirm({ title: t('deleteProviderConfirm', { slug: provider.slug }), body: t('deleteProviderBody'), confirmLabel: t('delete'), danger: true })) return false;
    await api<null>('DELETE', '/model-providers/' + provider.id);
    return true;
  }, async done => {
    if (!done) return;
    await client.invalidateQueries({ queryKey: ['model-providers'] });
    void navigate('/models/providers');
  });
  return (
    <section className="card form narrow" aria-label={t('dangerZone')}>
      <h2>{t('dangerZone')}</h2>
      <p className="muted">{t('deleteProviderBody')}</p>
      <SubmitError submit={submit} />
      <div className="row"><button type="button" className="danger" disabled={submit.busy} onClick={() => { void submit.run(); }}>{t('deleteProvider')}</button></div>
      {dialog}
    </section>
  );
}

export default function ProviderPage() {
  const { providerId } = useParams();
  const client = useQueryClient();
  const sections = useSections();
  const valid = isUuid(providerId);
  const query = useQuery({ queryKey: ['model-provider', providerId], queryFn: () => getWithETag<Provider>('/model-providers/' + providerId), enabled: valid });
  if (!valid) return <Navigate to="/models/providers" replace />;
  return (
    <>
      <div className="crumbs"><Link to="/models/providers">{t('tabProviders')}</Link></div>
      <Async query={query}>
        {loaded => {
          const provider = loaded.data;
          const etag = loaded.etag;
          const patch = (body: Update) => {
            if (!etag) return Promise.reject(new Error(t('errNoETag')));
            return api<Provider>('PATCH', '/model-providers/' + provider.id, body, { ifMatch: etag });
          };
          const saved = (name: string) => async () => {
            sections.markSaved(name);
            await Promise.all([
              client.invalidateQueries({ queryKey: ['model-provider', providerId] }),
              client.invalidateQueries({ queryKey: ['model-providers'] }),
            ]);
          };
          const version = (etag ?? '') + ':' + sections.nonce;
          const props = (name: string): FormProps => ({ provider, patch, onChange: () => sections.touch(name), onSaved: saved(name) });
          const managed = provider.scope === 'platform';
          return (
            <Page title={provider.slug}>
              <section className="card">
                <KV pairs={[
                  [t('adapter'), <span className="mono">{provider.adapter}</span>],
                  [t('scope'), <Badge tone={managed ? 'info' : undefined}>{tOr('scope_' + (provider.scope ?? ''), provider.scope ?? '—')}</Badge>],
                  [t('status'), <Badge tone={provider.enabled ? 'ok' : 'warn'}>{provider.enabled ? t('enabled') : t('disabled')}</Badge>],
                  [t('credential'), provider.has_credential ? t('credentialSet') : t('credentialNone')],
                  [t('id'), <span className="mono">{provider.id}</span>],
                ]} />
              </section>
              {managed ? <Notice kind="warn">{t('platformManaged')}</Notice> : (
                <>
                  <Section name="enabled" title={t('status')} sections={sections} narrow>
                    <EnabledForm key={version} {...props('enabled')} />
                  </Section>
                  <Section name="credential" title={t('credential')} sections={sections} narrow>
                    <CredentialForm key={version} {...props('credential')} />
                  </Section>
                  <Section name="settings" title={t('advancedSettings')} sections={sections}>
                    <SettingsForm key={version} settings={provider.settings} save={settings => patch({ settings })} onChange={() => sections.touch('settings')} onSaved={saved('settings')} />
                  </Section>
                  <DeleteSection provider={provider} />
                </>
              )}
            </Page>
          );
        }}
      </Async>
    </>
  );
}
