import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { get, type Page as PageOf, type Schemas } from '@/api/client';
import { ApprovalTable } from '@/components/approval-table';
import { Async } from '@/components/async';
import { Page } from '@/components/ui';
import { t } from '@/i18n';
import { store, stored } from '@/lib/storage';

const FILTER_KEY = 'hq.approvals.filter';

export default function ApprovalsPage() {
  const [filter, setFilter] = useState(() => (stored(FILTER_KEY, 'pending') === 'all' ? 'all' : 'pending'));
  const query = useQuery({
    queryKey: ['approvals', 'list', filter],
    queryFn: () => get<PageOf<Schemas['Approval']>>('/approvals?limit=100' + (filter === 'pending' ? '&status=pending' : '')),
    // Danh sách đang chờ tự làm mới; danh sách đầy đủ thì không.
    refetchInterval: filter === 'pending' ? 10_000 : false,
  });
  const pick = (
    <select aria-label={t('status')} value={filter} onChange={e => { store(FILTER_KEY, e.target.value); setFilter(e.target.value === 'all' ? 'all' : 'pending'); }}>
      <option value="pending">{t('filterPending')}</option>
      <option value="all">{t('filterAll')}</option>
    </select>
  );
  return (
    <Page title={t('navApprovals')} lead={t('approvalsLead')} actions={pick}>
      <Async query={query}>{r => <ApprovalTable rows={r.data} />}</Async>
    </Page>
  );
}
