// Chú giải: mỗi kiểu nét và mỗi loại node nghĩa là gì.
import { Icon, type IconName } from '@/components/icon';
import { t } from '@/i18n';
import { KINDS, type EdgeState, type NodeKind } from './model';

const ICON: Record<NodeKind, IconName> = { control: 'layers', agent: 'bot', server: 'server', tool: 'tool', system: 'globe' };
const STATES: readonly EdgeState[] = ['ok', 'approval', 'denied', 'broken'];

export function MapLegend() {
  return (
    <div className="map-panel map-legend" role="note" aria-label={t('mapLegend')}>
      <h3>{t('mapLegendNodes')}</h3>
      <ul>
        {KINDS.map(kind => <li key={kind}><span className="map-node-icon" data-kind={kind}><Icon name={ICON[kind]} size={13} /></span>{t(`nodeKind_${kind}`)}</li>)}
      </ul>
      <h3>{t('mapLegendEdges')}</h3>
      <ul>
        {STATES.map(state => (
          <li key={state}>
            <svg className="map-legend-line" viewBox="0 0 36 8" aria-hidden="true"><path d="M1 4h34" className={'map-edge state-' + state} /></svg>
            {t(`edge_${state}`)}
          </li>
        ))}
        <li>
          <svg className="map-legend-line" viewBox="0 0 36 8" aria-hidden="true"><path d="M1 4h34" className="map-edge state-ok live" /><circle cx="18" cy="4" r="3" className="map-edge-dot still" /></svg>
          {t('mapLegendLive')}
        </li>
      </ul>
      <p className="small muted">{t('mapLegendNote')}</p>
    </div>
  );
}
