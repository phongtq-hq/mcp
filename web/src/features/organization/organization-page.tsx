import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { api, getWithETag, type Schemas, type WithETag } from '@/api/client';
import { Async } from '@/components/async';
import { Form, TextField } from '@/components/form';
import { Section, useSections } from '@/components/sections';
import { SettingsForm } from '@/components/settings-form';
import { KV, Page, StatusBadge } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t } from '@/i18n';
import { fmtTime } from '@/lib/format';

type Organization = Schemas['Organization'];
type Update = Schemas['OrganizationUpdate'];

function NameForm({ org, patch, onChange, onSaved }: { org: Organization; patch: (body: Update) => Promise<Organization>; onChange: () => void; onSaved: () => Promise<void> }) {
  const [name, setName] = useState(org.name);
  const submit = useSubmit(() => patch({ name: name.trim() }), onSaved);
  return (
    <Form submit={submit}>
      <TextField label={t('name')} required value={name} onChange={v => { setName(v); onChange(); }} error={submit.fields.name} maxLength={200} />
    </Form>
  );
}

export default function OrganizationPage() {
  const client = useQueryClient();
  const sections = useSections();
  const query = useQuery({ queryKey: ['organization', 'settings'], queryFn: () => getWithETag<Organization>('/organization') });
  return (
    <Page title={t('navOrganization')} lead={t('organizationLead')}>
      <Async query={query}>
        {(loaded: WithETag<Organization>) => {
          const org = loaded.data;
          const etag = loaded.etag;
          // Không có ETag thì không sửa (xem project-settings-tab.tsx).
          const patch = (body: Update) => {
            if (!etag) return Promise.reject(new Error(t('errNoETag')));
            return api<Organization>('PATCH', '/organization', body, { ifMatch: etag });
          };
          const saved = (name: string) => async () => {
            sections.markSaved(name);
            await Promise.all([
              client.invalidateQueries({ queryKey: ['organization'] }),
              client.invalidateQueries({ queryKey: ['overview'] }),
            ]);
          };
          const version = (etag ?? '') + ':' + sections.nonce;
          return (
            <>
              <section className="card">
                <KV pairs={[
                  [t('slug'), <span className="mono">{org.slug}</span>],
                  [t('id'), <span className="mono">{org.id}</span>],
                  [t('status'), <StatusBadge status={org.status} />],
                  [t('createdAt'), fmtTime(org.created_at)],
                ]} />
              </section>
              <Section name="name" title={t('generalInfo')} sections={sections} narrow>
                <NameForm key={version} org={org} patch={patch} onChange={() => sections.touch('name')} onSaved={saved('name')} />
              </Section>
              <Section name="settings" title={t('advancedSettings')} sections={sections}>
                <SettingsForm
                  key={version}
                  settings={org.settings}
                  save={settings => patch({ settings })}
                  onChange={() => sections.touch('settings')}
                  onSaved={saved('settings')}
                />
              </Section>
            </>
          );
        }}
      </Async>
    </Page>
  );
}
