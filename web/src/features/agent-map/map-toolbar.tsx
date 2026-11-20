// Thanh công cụ nổi ở đáy canvas: chế độ con trỏ, thu phóng, bố cục và các bảng phụ.
import { Panel, useReactFlow } from '@/components/graph';
import { Icon, type IconName } from '@/components/icon';
import { t } from '@/i18n';
import { fitDuration, type Mode } from './agent-map-canvas';

interface Props {
  mode: Mode;
  onMode: (mode: Mode) => void;
  filters: boolean;
  filtersActive: boolean;
  onFilters: () => void;
  legend: boolean;
  onLegend: () => void;
  allExpanded: boolean;
  onExpandAll: () => void;
  onResetLayout: () => void;
  onShowAll: () => void;
}

function Tool({ icon, label, pressed, mark, onClick }: { icon: IconName; label: string; pressed?: boolean; mark?: boolean; onClick: () => void }) {
  return (
    <button type="button" className="map-tool" title={label} aria-label={label} aria-pressed={pressed} onClick={onClick}>
      <Icon name={icon} />{mark ? <span className="dot" aria-hidden="true" /> : null}
    </button>
  );
}

export function MapToolbar(p: Props) {
  const flow = useReactFlow();
  const duration = fitDuration();
  return (
    <Panel position="bottom-center">
      <div className="map-toolbar" role="toolbar" aria-label={t('mapToolbar')}>
        <Tool icon="pointer" label={t('mapModeSelect')} pressed={p.mode === 'select'} onClick={() => p.onMode('select')} />
        <Tool icon="hand" label={t('mapModePan')} pressed={p.mode === 'pan'} onClick={() => p.onMode('pan')} />
        <span className="sep" aria-hidden="true" />
        <Tool icon="minus" label={t('mapZoomOut')} onClick={() => { void flow.zoomOut({ duration }); }} />
        <Tool icon="plus" label={t('mapZoomIn')} onClick={() => { void flow.zoomIn({ duration }); }} />
        <Tool icon="fit" label={t('mapFit')} onClick={() => { void flow.fitView({ duration, padding: 0.15 }); }} />
        <span className="sep" aria-hidden="true" />
        <Tool icon="filter" label={t('mapFilters')} pressed={p.filters} mark={p.filtersActive} onClick={p.onFilters} />
        <Tool icon="eye" label={t('mapLegend')} pressed={p.legend} onClick={p.onLegend} />
        <Tool icon="expand" label={t(p.allExpanded ? 'mapCollapseAll' : 'mapExpandAll')} pressed={p.allExpanded} onClick={p.onExpandAll} />
        <Tool icon="refresh" label={t('mapResetLayout')} onClick={p.onResetLayout} />
        <span className="sep" aria-hidden="true" />
        <button type="button" className="map-tool text" onClick={p.onShowAll}>{t('mapShowAll')}</button>
      </div>
    </Panel>
  );
}
