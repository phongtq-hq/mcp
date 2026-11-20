import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, get, getWithETag, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { RoutingForm } from '@/components/routing-form';
import { Section, useSections } from '@/components/sections';
import { Page } from '@/components/ui';
import { t } from '@/i18n';
import { ModelsTabs } from './models-tabs';

type Organization = Schemas['Organization'];
type Model = Schemas['Model'];

// Định tuyến theo tier của cả organization. Dự án ghi đè theo từng tier ở tab Cài đặt của nó.
export default function RoutingPage() {
  const client = useQueryClient();
  const sections = useSections();
  const query = useQuery({
    queryKey: ['organization', 'routing'],
    queryFn: async () => {
      const [org, models] = await Promise.all([
        getWithETag<Organization>('/organization'),
        soft(get<PageOf<Model>>('/models?enabled=true&limit=200')),
      ]);
      return { org, models: models ? models.data.map(m => ({ value: m.id, label: m.display_name })) : null, truncated: !!models?.next_cursor };
    },
  });
  return (
    <Page title={t('navModels')} lead={t('routingLead')}>
      <ModelsTabs current="routing" />
      <Async query={query}>
        {({ org, models, truncated }) => {
          const etag = org.etag;
          const save = (settings: Record<string, unknown>) => {
            if (!etag) return Promise.reject(new Error(t('errNoETag')));
            return api<Organization>('PATCH', '/organization', { settings }, { ifMatch: etag });
          };
          const saved = async () => {
            sections.markSaved('routing');
            await client.invalidateQueries({ queryKey: ['organization'] });
          };
          return (
            <Section name="routing" title={t('tabRouting')} sections={sections}>
              <RoutingForm key={(etag ?? '') + ':' + sections.nonce} settings={org.data.settings} models={models} truncated={truncated} save={save} onChange={() => sections.touch('routing')} onSaved={saved} />
            </Section>
          );
        }}
      </Async>
    </Page>
  );
}
