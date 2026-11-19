import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { api, type Schemas } from '@/api/client';
import { Form, SelectField, TextField } from '@/components/form';
import { Page } from '@/components/ui';
import { useSubmit } from '@/components/use-submit';
import { t } from '@/i18n';

type Project = Schemas['Project'];
type Environment = Schemas['Environment'];

export default function ProjectNewPage() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');
  const [environment, setEnvironment] = useState<Environment | ''>('');
  const submit = useSubmit(
    () => {
      // Ô chọn là bắt buộc nên trình duyệt đã chặn trường hợp này; kiểm lại để kiểu của thân đúng.
      if (environment === '') throw new Error(t('envRequired'));
      const body: Schemas['ProjectCreate'] = { name: name.trim(), slug: slug.trim(), environment };
      return api<Project>('POST', '/projects', body);
    },
    async project => {
      await Promise.all([client.invalidateQueries({ queryKey: ['projects'] }), client.invalidateQueries({ queryKey: ['overview'] })]);
      void navigate('/projects/' + project.id);
    },
  );
  return (
    <>
      <div className="crumbs"><Link to="/projects">{t('navProjects')}</Link> / {t('newProject')}</div>
      <Page title={t('newProject')}>
        <div className="card form narrow">
          <Form submit={submit} submitLabel={t('create')} onCancel={() => { void navigate('/projects'); }}>
            <TextField label={t('name')} required value={name} onChange={setName} error={submit.fields.name} maxLength={200} />
            <TextField label={t('slug')} required mono value={slug} onChange={setSlug} error={submit.fields.slug} hint={t('slugHint')} maxLength={63} />
            <SelectField
              label={t('environment')}
              required
              value={environment}
              onChange={v => setEnvironment(v === 'prod' || v === 'non_prod' ? v : '')}
              placeholder={t('choose')}
              options={[{ value: 'non_prod', label: t('envNonProd') }, { value: 'prod', label: t('envProd') }]}
              error={submit.fields.environment}
              hint={t('envHint')}
            />
          </Form>
        </div>
      </Page>
    </>
  );
}
