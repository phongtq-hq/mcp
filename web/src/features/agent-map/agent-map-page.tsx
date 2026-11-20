// Bản đồ Agent: Control Plane → Agent → MCP server → tool → hệ thống đích, vẽ từ dữ liệu thật
// của Management API. Trang giữ trạng thái của bản đồ (bộ lọc, lựa chọn, server đang mở, lần
// chạy đang theo dõi); canvas chỉ vẽ.
import { useQuery } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router';
import type { Schemas } from '@/api/client';
import { Async } from '@/components/async';
import { Panel, ReactFlowProvider, useReactFlow } from '@/components/graph';
import { Icon } from '@/components/icon';
import { Notice, StatusBadge } from '@/components/ui';
import { t } from '@/i18n';
import { shortId } from '@/lib/format';
import { useTitle } from '@/lib/use-title';
import { buildGraph } from './adapter';
import { AgentMapCanvas, fitDuration, type Mode } from './agent-map-canvas';
import { MapFilters } from './map-filters';
import { MapLegend } from './map-legend';
import { MapToolbar } from './map-toolbar';
import type { GraphModel, GraphNode } from './model';
import { NodeInspector } from './node-inspector';
import { MapActions } from './nodes';
import { loadActivity, loadRunning, loadTopology } from './source';
import { applyFilters, DEFAULT_FILTERS, defaultExpanded, filtersActive, layout, liveFocus, related, search, type MapFilters as Filters } from './view';

type Execution = Schemas['Execution'];

const RUNNING_EVERY_MS = 10_000;
const ACTIVITY_EVERY_MS = 6_000;

function Search({ model, onPick }: { model: GraphModel; onPick: (node: GraphNode) => void }) {
  const [text, setText] = useState('');
  const found = useMemo(() => search(model.nodes, text), [model.nodes, text]);
  return (
    <div className="map-search">
      <label className="map-search-box">
        <Icon name="search" />
        <span className="sr-only">{t('mapSearch')}</span>
        <input type="search" value={text} placeholder={t('mapSearch')} onChange={e => setText(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter' && found[0]) { onPick(found[0]); setText(''); } }} />
      </label>
      {text.trim() ? (
        <ul className="map-panel map-search-results">
          {found.map(node => (
            <li key={node.id}>
              <button type="button" onClick={() => { onPick(node); setText(''); }}>
                <span>{node.label}</span><span className="small muted">{t(`nodeKind_${node.kind}`)}</span>
              </button>
            </li>
          ))}
          {found.length === 0 ? <li className="muted small">{t('mapSearchEmpty')}</li> : null}
        </ul>
      ) : null}
    </div>
  );
}

function LivePanel({ running, watching, onWatch }: { running: Execution[] | undefined; watching: string | null; onWatch: (id: string | null) => void }) {
  const list = running ?? [];
  return (
    <div className="map-panel map-live" role="region" aria-label={t('mapLive')}>
      <h3><span className={list.length ? 'pulse on' : 'pulse'} aria-hidden="true" />{t('mapLiveCount', { count: list.length })}</h3>
      {list.length === 0 ? <p className="small muted">{t('mapLiveNone')}</p> : (
        <ul>
          {list.map(x => (
            <li key={x.id}>
              <button type="button" aria-pressed={watching === x.id} onClick={() => onWatch(watching === x.id ? null : x.id)}>
                <span className="mono">{shortId(x.id)}</span><StatusBadge status={x.status} />
              </button>
              <Link className="small" to={'/executions/' + x.id}>{t('mapOpenExecution')}</Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Workspace({ model, refreshing, onRefresh }: { model: GraphModel; refreshing: boolean; onRefresh: () => void }) {
  const flow = useReactFlow();
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS);
  const [expanded, setExpanded] = useState(() => defaultExpanded(model));
  const [mode, setMode] = useState<Mode>('pan');
  const [selection, setSelection] = useState<{ id: string | null; count: number }>({ id: null, count: 0 });
  const [showFilters, setShowFilters] = useState(false);
  const [showLegend, setShowLegend] = useState(false);
  const [layoutNonce, setLayoutNonce] = useState(0);
  const [watching, setWatching] = useState<string | null>(null);
  // Node cần đưa vào giữa khung nhìn sau khi lớp nhìn đã có nó (vừa mở server, vừa bỏ lọc).
  const [pending, setPending] = useState<string | null>(null);

  const view = useMemo(() => applyFilters(model, filters, expanded), [model, filters, expanded]);
  const positions = useMemo(() => layout(view), [view]);
  const focus = useMemo(() => related(model, view, selection.id), [model, view, selection.id]);

  const projects = useMemo(() => {
    const seen = new Map<string, string>();
    for (const n of model.nodes) if (n.kind === 'agent') seen.set(n.projectId, n.projectName);
    return [...seen].map(([id, name]) => ({ id, name }));
  }, [model.nodes]);
  const statuses = useMemo(() => [...new Set(model.nodes.flatMap(n => (n.kind === 'agent' ? [n.status] : [])))], [model.nodes]);
  const servers = useMemo(() => model.nodes.filter(n => n.kind === 'server').map(n => n.id), [model.nodes]);
  const allExpanded = servers.length > 0 && servers.every(id => expanded.has(id));

  const projectIds = useMemo(() => projects.map(p => p.id), [projects]);
  const running = useQuery({
    queryKey: ['agent-map', 'running', projectIds],
    queryFn: () => loadRunning(projectIds),
    enabled: projectIds.length > 0,
    refetchInterval: RUNNING_EVERY_MS,
  });
  // Lần chạy đang theo dõi đã kết thúc (không còn trong danh sách đang chạy) thì thôi theo dõi.
  const watched = watching && running.data?.some(x => x.id === watching) ? watching : null;
  const activity = useQuery({
    queryKey: ['agent-map', 'activity', watched],
    queryFn: () => loadActivity(watched ?? ''),
    enabled: !!watched,
    refetchInterval: ACTIVITY_EVERY_MS,
  });
  const live = useMemo(
    () => (watched && activity.data ? liveFocus(model, view, activity.data.agentIds, activity.data.toolIds) : null),
    [watched, activity.data, model, view],
  );

  const toggle = useCallback((serverNode: string) => {
    setExpanded(current => {
      const next = new Set(current);
      if (next.has(serverNode)) next.delete(serverNode); else next.add(serverNode);
      return next;
    });
  }, []);
  const actions = useMemo(() => ({ toggle }), [toggle]);

  const onSelect = useCallback((id: string | null, count: number) => setSelection({ id, count }), []);

  // pick: chọn một node từ ngoài canvas (ô tìm, inspector). Node chưa có trên lớp nhìn thì mở
  // server chứa nó, hoặc bỏ bộ lọc đang che nó, rồi mới đưa nó vào giữa.
  const pick = useCallback((id: string) => {
    const node = model.nodes.find(n => n.id === id);
    if (!node) return;
    if (node.kind === 'tool') setExpanded(current => (current.has(node.serverNode) ? current : new Set(current).add(node.serverNode)));
    setPending(id);
  }, [model.nodes]);

  useEffect(() => {
    if (!pending) return;
    if (!view.nodes.some(n => n.id === pending)) {
      // Vẫn chưa hiện sau khi đã mở server: bộ lọc đang che nó.
      const handle = setTimeout(() => setFilters(DEFAULT_FILTERS), 0);
      return () => clearTimeout(handle);
    }
    const handle = requestAnimationFrame(() => {
      flow.setNodes(nodes => nodes.map(n => ({ ...n, selected: n.id === pending })));
      setSelection({ id: pending, count: 1 });
      void flow.fitView({ nodes: [{ id: pending }], duration: fitDuration(), maxZoom: 1.1, padding: 0.6 });
      setPending(null);
    });
    return () => cancelAnimationFrame(handle);
  }, [pending, view.nodes, flow]);

  const close = useCallback(() => {
    flow.setNodes(nodes => nodes.map(n => (n.selected ? { ...n, selected: false } : n)));
    setSelection({ id: null, count: 0 });
  }, [flow]);

  const empty = model.nodes.length <= 1;

  return (
    <div className="map-page">
      <div className="map-stage">
        <MapActions.Provider value={actions}>
          <AgentMapCanvas view={view} positions={positions} focus={focus} live={live} selected={selection.id}
            expanded={expanded} mode={mode} layoutNonce={layoutNonce} onSelect={onSelect}>
            <Panel position="top-left">
              <div className="map-top">
                <Search model={model} onPick={node => pick(node.id)} />
                <button type="button" className="map-refresh" disabled={refreshing} onClick={onRefresh}><Icon name="refresh" />{t('refresh')}</button>
                {showFilters ? <MapFilters filters={filters} projects={projects} statuses={statuses} onChange={setFilters} /> : null}
              </div>
            </Panel>
            <Panel position="top-right">
              <div className="map-side">
                <LivePanel running={running.data} watching={watched} onWatch={setWatching} />
                {showLegend ? <MapLegend /> : null}
              </div>
            </Panel>
            <MapToolbar
              mode={mode} onMode={setMode}
              filters={showFilters} filtersActive={filtersActive(filters)} onFilters={() => setShowFilters(v => !v)}
              legend={showLegend} onLegend={() => setShowLegend(v => !v)}
              allExpanded={allExpanded} onExpandAll={() => setExpanded(allExpanded ? new Set() : new Set(servers))}
              onResetLayout={() => setLayoutNonce(n => n + 1)}
              onShowAll={() => { setFilters(DEFAULT_FILTERS); setExpanded(new Set(servers)); }}
            />
          </AgentMapCanvas>
        </MapActions.Provider>
        <div className="map-notes">
          {model.truncated ? <Notice kind="warn">{t('mapTruncated')}</Notice> : null}
          {model.unread > 0 ? <Notice kind="warn">{t('mapUnread', { count: model.unread })}</Notice> : null}
          {empty ? <Notice kind="warn">{t('mapEmpty')} <Link to="/projects">{t('navProjects')}</Link></Notice> : null}
          {!empty && view.nodes.length <= 1 ? <Notice kind="warn">{t('mapAllFiltered')}</Notice> : null}
        </div>
      </div>
      {selection.id ? <NodeInspector model={model} selected={selection.id} count={selection.count} onSelect={pick} onClose={close} /> : null}
    </div>
  );
}

export default function AgentMapPage() {
  useTitle(t('navAgentMap'));
  const query = useQuery({ queryKey: ['agent-map', 'topology'], queryFn: loadTopology });
  const model = useMemo(() => (query.data ? buildGraph(query.data) : null), [query.data]);
  return (
    <div className="map-root">
      <Async query={query} loading="detail">
        {() => (model ? (
          <ReactFlowProvider>
            <Workspace model={model} refreshing={!!query.isFetching} onRefresh={() => { void query.refetch(); }} />
          </ReactFlowProvider>
        ) : null)}
      </Async>
    </div>
  );
}
