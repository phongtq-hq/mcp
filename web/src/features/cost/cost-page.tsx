import { useQuery } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { get, soft, type Page as PageOf, type Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { DataTable } from '@/components/data-table';
import { Count, MoneyCount } from '@/components/count';
import { Notice, Page, StatCard } from '@/components/ui';
import { t, type Key } from '@/i18n';
import { fmtMoney, fmtNumber, shortId } from '@/lib/format';
import { store, stored } from '@/lib/storage';

type Summary = Schemas['UsageSummary'];
type Group = Summary['groups'][number];

const RANGES: readonly (readonly [string, Key])[] = [['7', 'range7'], ['30', 'range30'], ['month', 'rangeMonth']];
const DIMS: readonly (readonly [string, Key])[] = [['agent', 'dimAgent'], ['model', 'dimModel'], ['project', 'dimProject'], ['user', 'dimUser']];

function pick(options: readonly (readonly [string, Key])[], value: string, fallback: string): string {
  return options.some(([v]) => v === value) ? value : fallback;
}

function costRange(kind: string): [Date, Date] {
  const now = new Date();
  const to = new Date(now.getTime() + 60_000);
  if (kind === 'month') return [new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1)), to];
  return [new Date(now.getTime() - (kind === '7' ? 7 : 30) * 86_400_000), to];
}

// names: bảng tra id → tên cho chiều đang nhóm, để bảng không hiện toàn UUID. Thiếu
// quyền đọc một danh sách thì chiều đó hiện id rút gọn.
async function names(dim: string): Promise<Map<string, string>> {
  const map = new Map<string, string>();
  if (dim === 'model') {
    const r = await soft(get<PageOf<Schemas['Model']>>('/models?limit=200'));
    for (const m of (r ? r.data : [])) map.set(m.id, m.display_name || m.provider_model);
  } else if (dim === 'project' || dim === 'agent') {
    const r = await soft(get<PageOf<Schemas['Project']>>('/projects?limit=200'));
    const projects = r ? r.data : [];
    for (const p of projects) map.set(p.id, p.name);
    if (dim === 'agent') {
      // Trần: tên Agent chỉ tra ở 20 dự án đầu.
      const lists = await Promise.all(projects.slice(0, 20).map(p => soft(get<PageOf<Schemas['Agent']>>(`/projects/${p.id}/agents?limit=200`))));
      for (const l of lists) for (const a of (l ? l.data : [])) map.set(a.id, a.name);
    }
  }
  return map;
}

async function load(range: string, dim: string) {
  const [from, to] = costRange(range);
  const [summary, lookup] = await Promise.all([
    get<Summary>(`/usage/summary?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}&group_by=${dim}`),
    names(dim),
  ]);
  return { summary, lookup };
}

// Thanh tỉ trọng. Độ rộng đặt qua thuộc tính style của React (CSSOM), cách mà CSP
// `style-src 'self'` cho phép; không có style nội tuyến trong HTML.
function ShareBar({ percent }: { percent: number }) {
  return <div className="bar" role="img" aria-label={percent.toFixed(0) + '%'}><span style={{ width: percent.toFixed(1) + '%' }} /></div>;
}

export default function CostPage() {
  const [range, setRange] = useState(() => pick(RANGES, stored('hq.cost.range', 'month'), 'month'));
  const [dim, setDim] = useState(() => pick(DIMS, stored('hq.cost.dim', 'agent'), 'agent'));
  const query = useQuery({ queryKey: ['cost', range, dim], queryFn: () => load(range, dim) });
  const dimLabel = t(DIMS.find(([v]) => v === dim)?.[1] ?? 'dimAgent');

  const controls = (
    <>
      <label>
        {t('range')}
        <select value={range} onChange={e => { store('hq.cost.range', e.target.value); setRange(pick(RANGES, e.target.value, 'month')); }}>
          {RANGES.map(([v, k]) => <option key={v} value={v}>{t(k)}</option>)}
        </select>
      </label>
      <label>
        {t('groupBy')}
        <select value={dim} onChange={e => { store('hq.cost.dim', e.target.value); setDim(pick(DIMS, e.target.value, 'agent')); }}>
          {DIMS.map(([v, k]) => <option key={v} value={v}>{t(k)}</option>)}
        </select>
      </label>
    </>
  );

  return (
    <Page title={t('navCost')} lead={t('costLead')} actions={controls}>
      <Async query={query} loading="detail">
        {({ summary: r, lookup }) => {
          // Tỉ trọng chỉ để vẽ thanh; mọi con số tiền hiển thị là số API trả về.
          const total = Number(r.totals.estimated_cost.amount) || 0;
          const share = (g: Group) => (total > 0 ? Math.max(0, Math.min(100, (Number(g.estimated_cost.amount) || 0) / total * 100)) : 0);
          const label = (g: Group): ReactNode => {
            const id = g.key ? g.key[dim] : null;
            if (!id) return <span className="muted">{t('ungrouped')}</span>;
            return lookup.get(id) || <span className="mono">{shortId(id)}</span>;
          };
          return (
            <>
              <div className="grid">
                <StatCard label={t('totalCost')} value={<MoneyCount money={r.totals.estimated_cost} />} />
                <StatCard label={t('calls')} value={<Count value={r.totals.calls ?? 0} format={fmtNumber} />} hint={`${t('executions')}: ${fmtNumber(r.totals.executions)}`} />
                <StatCard label={t('inputTokens')} value={<Count value={r.totals.input_tokens ?? 0} format={fmtNumber} />} />
                <StatCard label={t('outputTokens')} value={<Count value={r.totals.output_tokens ?? 0} format={fmtNumber} />} />
              </div>
              {(r.totals.unpriced_calls ?? 0) > 0 ? <Notice kind="warn">{t('unpricedCalls', { n: fmtNumber(r.totals.unpriced_calls) })}</Notice> : null}
              {r.truncated ? <Notice kind="warn">{t('truncatedGroups')}</Notice> : null}
              <DataTable
                rows={r.groups}
                rowKey={g => (g.key ? g.key[dim] : null) ?? ''}
                emptyText={t('noUsage')}
                columns={[
                  { label: dimLabel, cell: label },
                  { label: t('cost'), className: 'num', cell: g => fmtMoney(g.estimated_cost) },
                  { label: t('share'), cell: g => <ShareBar percent={share(g)} /> },
                  { label: t('calls'), className: 'num', cell: g => fmtNumber(g.calls) },
                  { label: t('inputTokens'), className: 'num', cell: g => fmtNumber(g.input_tokens) },
                  { label: t('outputTokens'), className: 'num', cell: g => fmtNumber(g.output_tokens) },
                  { label: t('avgPerExecution'), className: 'num', cell: g => g.avg_cost_per_execution ? fmtMoney(g.avg_cost_per_execution) : '—' },
                ]}
              />
            </>
          );
        }}
      </Async>
    </Page>
  );
}
