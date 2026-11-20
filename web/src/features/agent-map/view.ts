// Phần "nhìn" của bản đồ, vẫn thuần: lọc, thu gọn tool theo server, tìm node, tập liên quan
// của một lựa chọn và toạ độ. Không có gì ở đây phụ thuộc vào thư viện vẽ.
import { layeredLayout, type Point } from '@/lib/graph-layout';
import { nodeId, type GraphEdge, type GraphModel, type GraphNode, type NodeKind } from './model';

export interface MapFilters {
  // Loại node đang ẩn.
  hidden: readonly NodeKind[];
  environment: string;
  status: string;
  risk: string;
  projectId: string;
  // Ẩn Agent không nối tới server nào và server không Agent nào (đang hiện) dùng.
  hideDisconnected: boolean;
}

export const ALL = 'all';
export const DEFAULT_FILTERS: MapFilters = { hidden: [], environment: ALL, status: ALL, risk: ALL, projectId: ALL, hideDisconnected: false };

export function filtersActive(f: MapFilters): boolean {
  return f.hidden.length > 0 || f.hideDisconnected || [f.environment, f.status, f.risk, f.projectId].some(v => v !== ALL);
}

export interface View {
  nodes: GraphNode[];
  edges: GraphEdge[];
  // Số tool đang được thu gọn vào node server (server chưa mở).
  folded: Map<string, number>;
}

// Trên ngần này tool thì bản đồ mở ra với tool thu gọn vào server: người dùng mở server cần xem.
const EXPAND_ALL_UP_TO = 30;

export function defaultExpanded(model: GraphModel): Set<string> {
  const tools = model.nodes.filter(n => n.kind === 'tool').length;
  return tools <= EXPAND_ALL_UP_TO ? new Set(model.nodes.filter(n => n.kind === 'server').map(n => n.id)) : new Set();
}

export function applyFilters(model: GraphModel, filters: MapFilters, expanded: ReadonlySet<string>): View {
  const hidden = new Set(filters.hidden);
  const shown = new Map<string, GraphNode>();
  const is = (value: string, wanted: string) => wanted === ALL || value === wanted;

  for (const n of model.nodes) {
    if (n.kind !== 'agent' || hidden.has('agent')) continue;
    if (!is(n.environment, filters.environment) || !is(n.status, filters.status) || !is(n.projectId, filters.projectId)) continue;
    if (!is(n.risk ?? '', filters.risk)) continue;
    shown.set(n.id, n);
  }
  const used = new Set<string>();
  const linkedAgents = new Set<string>();
  for (const e of model.edges) {
    if (e.kind === 'uses' && shown.has(e.source)) { used.add(e.target); linkedAgents.add(e.source); }
    if (e.kind === 'delegates' && shown.has(e.source) && shown.has(e.target)) { linkedAgents.add(e.source); linkedAgents.add(e.target); }
  }
  if (filters.hideDisconnected) for (const id of [...shown.keys()]) if (!linkedAgents.has(id)) shown.delete(id);

  const folded = new Map<string, number>();
  const systems = new Set<string>();
  for (const n of model.nodes) {
    if (n.kind !== 'server' || hidden.has('server')) continue;
    if (filters.hideDisconnected && !used.has(n.id)) continue;
    shown.set(n.id, n);
    if (n.systemId) systems.add(n.systemId);
  }
  for (const n of model.nodes) {
    if (n.kind !== 'tool' || hidden.has('tool') || !is(n.risk, filters.risk)) continue;
    // Server đang ẩn theo loại node thì tool của nó vẫn hiện được; server bị lọc ra thì không.
    const server = shown.get(n.serverNode);
    if (!server && !hidden.has('server')) continue;
    if (filters.hideDisconnected && !used.has(n.serverNode)) continue;
    if (!expanded.has(n.serverNode)) { folded.set(n.serverNode, (folded.get(n.serverNode) ?? 0) + 1); continue; }
    shown.set(n.id, n);
  }
  for (const n of model.nodes) {
    if (n.kind === 'control' && !hidden.has('control')) shown.set(n.id, n);
    if (n.kind === 'system' && !hidden.has('system') && systems.has(n.id)) shown.set(n.id, n);
  }

  const edges = model.edges.filter(e => shown.has(e.source) && shown.has(e.target));
  // Đường tới hệ thống đích: đi từ tool khi tool của server đang hiện, không thì từ server.
  const toolsOf = new Map<string, string[]>();
  for (const n of shown.values()) {
    if (n.kind !== 'tool') continue;
    const list = toolsOf.get(n.serverNode);
    if (list) list.push(n.id); else toolsOf.set(n.serverNode, [n.id]);
  }
  for (const n of shown.values()) {
    if (n.kind !== 'server' || !n.systemId || !shown.has(n.systemId)) continue;
    const from = toolsOf.get(n.id) ?? [n.id];
    for (const source of from) edges.push({ id: `connects:${source}`, source, target: n.systemId, kind: 'connects', state: n.tone === 'bad' ? 'broken' : 'ok' });
  }
  return { nodes: [...shown.values()], edges, folded };
}

export interface Focus {
  nodes: Set<string>;
  edges: Set<string>;
  // Cạnh server → tool mà cấu hình của Agent đang chọn từ chối.
  denied: Set<string>;
}

// related: những gì liên quan tới node / cạnh đang chọn; phần còn lại của bản đồ mờ đi.
// null = không làm mờ gì (không chọn gì, hoặc chọn node gốc).
export function related(model: GraphModel, view: View, selected: string | null): Focus | null {
  if (!selected || selected === nodeId.control) return null;
  const focus: Focus = { nodes: new Set(), edges: new Set(), denied: new Set() };
  const byId = new Map(view.nodes.map(n => [n.id, n]));
  const edge = view.edges.find(e => e.id === selected);
  if (edge) {
    focus.edges.add(edge.id);
    focus.nodes.add(edge.source).add(edge.target);
    return focus;
  }
  const node = byId.get(selected);
  if (!node) return null;
  focus.nodes.add(node.id);

  // Các Agent "đi qua" lựa chọn này, và với mỗi Agent là các tool nó chạm tới.
  const agents = new Set<string>();
  const tools = new Set<string>();
  const servers = new Set<string>();
  if (node.kind === 'agent') {
    agents.add(node.id);
    for (const [tool, decision] of model.access.get(node.id) ?? []) {
      if (!byId.has(tool)) continue;
      tools.add(tool);
      if (decision === 'deny') focus.denied.add('exposes:' + tool.slice('tool:'.length));
    }
  } else if (node.kind === 'tool') {
    tools.add(node.id);
    servers.add(node.serverNode);
    for (const [agent, decisions] of model.access) {
      const decision = decisions.get(node.id);
      if (decision && decision !== 'deny' && byId.has(agent)) agents.add(agent);
    }
  } else if (node.kind === 'server') {
    servers.add(node.id);
  } else if (node.kind === 'system') {
    for (const n of view.nodes) if (n.kind === 'server' && n.systemId === node.id) servers.add(n.id);
  }

  if (node.kind === 'server' || node.kind === 'system') {
    for (const n of view.nodes) if (n.kind === 'tool' && servers.has(n.serverNode)) tools.add(n.id);
  }

  for (const e of view.edges) {
    const keep =
      (e.kind === 'manages' && agents.has(e.target)) ||
      (e.kind === 'uses' && (node.kind === 'agent' ? agents.has(e.source) : node.kind === 'tool' ? agents.has(e.source) && servers.has(e.target) : servers.has(e.target))) ||
      (e.kind === 'exposes' && tools.has(e.target)) ||
      (e.kind === 'delegates' && (agents.has(e.source) || agents.has(e.target)) && node.kind === 'agent') ||
      (e.kind === 'connects' && (e.target === node.id || tools.has(e.source) || servers.has(e.source)));
    if (!keep) continue;
    focus.edges.add(e.id);
    focus.nodes.add(e.source).add(e.target);
  }
  for (const id of [...agents, ...tools, ...servers]) focus.nodes.add(id);
  return focus;
}

// liveFocus: node và cạnh của một lần chạy đang diễn ra (các Agent của nó và tool nó đã gọi).
export function liveFocus(model: GraphModel, view: View, agentIds: readonly string[], toolIds: readonly string[]): Focus {
  const focus: Focus = { nodes: new Set(), edges: new Set(), denied: new Set() };
  const agents = new Set(agentIds.map(nodeId.agent));
  const tools = new Set(toolIds.map(nodeId.tool));
  const servers = new Set<string>();
  // Server của tool lấy từ model: tool đang thu gọn vào server vẫn làm sáng đường tới server đó.
  for (const n of model.nodes) if (n.kind === 'tool' && tools.has(n.id)) servers.add(n.serverNode);
  for (const e of view.edges) {
    const keep =
      (e.kind === 'manages' && agents.has(e.target)) ||
      (e.kind === 'uses' && agents.has(e.source) && servers.has(e.target)) ||
      (e.kind === 'exposes' && tools.has(e.target)) ||
      (e.kind === 'delegates' && agents.has(e.source) && agents.has(e.target));
    if (!keep) continue;
    focus.edges.add(e.id);
    focus.nodes.add(e.source).add(e.target);
  }
  for (const n of view.nodes) if (agents.has(n.id) || tools.has(n.id) || servers.has(n.id)) focus.nodes.add(n.id);
  return focus;
}

function fold(s: string): string {
  return s.normalize('NFD').replace(/[̀-ͯ]/g, '').replace(/đ/gi, 'd').toLowerCase();
}

// search: node khớp với chuỗi tìm (tên, slug, định danh), theo thứ tự của bản đồ.
export function search(nodes: readonly GraphNode[], query: string, limit = 8): GraphNode[] {
  const q = fold(query.trim());
  if (!q) return [];
  const text = (n: GraphNode): string => {
    if (n.kind === 'agent') return `${n.label} ${n.slug} ${n.agentId} ${n.projectName}`;
    if (n.kind === 'server') return `${n.label} ${n.slug} ${n.serverId}`;
    if (n.kind === 'tool') return `${n.label} ${n.toolId}`;
    return n.label;
  };
  return nodes.filter(n => fold(text(n)).includes(q)).slice(0, limit);
}

// Kích thước node (px) dùng cho bố cục; khớp với .map-node ở app.css.
export const NODE_SIZE: Record<NodeKind, { width: number; height: number }> = {
  control: { width: 236, height: 148 },
  agent: { width: 228, height: 82 },
  server: { width: 228, height: 82 },
  tool: { width: 204, height: 52 },
  system: { width: 196, height: 52 },
};

const RANK: Record<NodeKind, number> = { control: 0, agent: 1, server: 2, tool: 3, system: 4 };

export function layout(view: View): Map<string, Point> {
  return layeredLayout(
    view.nodes.map(n => ({ id: n.id, rank: RANK[n.kind], ...NODE_SIZE[n.kind] })),
    view.edges,
    { columnGap: 110, rowGap: 14 },
  );
}
