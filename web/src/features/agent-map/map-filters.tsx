// Bộ lọc của bản đồ. Lọc chạy trên ảnh chụp đã tải (bản đồ cần cả đồ thị để vẽ quan hệ),
// không phải trên một danh sách phân trang.
import { t, tOr } from '@/i18n';
import { KINDS, RISKS, type NodeKind } from './model';
import { ALL, DEFAULT_FILTERS, type MapFilters as Filters } from './view';

interface Props {
  filters: Filters;
  projects: readonly { id: string; name: string }[];
  statuses: readonly string[];
  onChange: (filters: Filters) => void;
}

export function MapFilters({ filters, projects, statuses, onChange }: Props) {
  const set = (patch: Partial<Filters>) => onChange({ ...filters, ...patch });
  const toggleKind = (kind: NodeKind) => set({ hidden: filters.hidden.includes(kind) ? filters.hidden.filter(k => k !== kind) : [...filters.hidden, kind] });
  return (
    <form className="map-panel map-filters" aria-label={t('mapFilters')} onSubmit={e => e.preventDefault()}>
      <label>
        {t('project')}
        <select value={filters.projectId} onChange={e => set({ projectId: e.target.value })}>
          <option value={ALL}>{t('filterAll')}</option>
          {projects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
        </select>
      </label>
      <label>
        {t('environment')}
        <select value={filters.environment} onChange={e => set({ environment: e.target.value })}>
          <option value={ALL}>{t('filterAll')}</option>
          <option value="prod">{t('envProd')}</option>
          <option value="non_prod">{t('envNonProd')}</option>
        </select>
      </label>
      <label>
        {t('mapAgentStatus')}
        <select value={filters.status} onChange={e => set({ status: e.target.value })}>
          <option value={ALL}>{t('filterAll')}</option>
          {statuses.map(s => <option key={s} value={s}>{tOr('st_' + s, s)}</option>)}
        </select>
      </label>
      <label>
        {t('risk')}
        <select value={filters.risk} onChange={e => set({ risk: e.target.value })}>
          <option value={ALL}>{t('filterAll')}</option>
          {RISKS.map(r => <option key={r} value={r}>{tOr('risk_' + r, r)}</option>)}
        </select>
      </label>
      <fieldset className="plain">
        <legend>{t('mapNodeTypes')}</legend>
        {KINDS.map(kind => (
          <label key={kind} className="inline">
            <input type="checkbox" checked={!filters.hidden.includes(kind)} onChange={() => toggleKind(kind)} />{t(`nodeKind_${kind}`)}
          </label>
        ))}
      </fieldset>
      <label className="inline">
        <input type="checkbox" checked={filters.hideDisconnected} onChange={e => set({ hideDisconnected: e.target.checked })} />{t('mapHideDisconnected')}
      </label>
      <button type="button" onClick={() => onChange(DEFAULT_FILTERS)}>{t('mapResetFilters')}</button>
    </form>
  );
}
