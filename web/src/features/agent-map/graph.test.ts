import { describe, expect, it } from 'vitest';
import { buildGraph, endpointHost, maxRisk, resolveAccess } from './adapter';
import { topology } from './fixtures';
import { nodeId, type AgentNode, type ServerNode } from './model';
import { ALL, applyFilters, DEFAULT_FILTERS, defaultExpanded, layout, liveFocus, NODE_SIZE, related, search } from './view';

const server = '00000000-0000-4000-8002-000000000000';
const ref = (patch: object) => ({ server_id: server, allow: ['*'], require_approval: 'policy' as const, ...patch });
const tool = (name: string) => ({ server_id: server, name });

describe('resolveAccess (cùng luật với Gateway)', () => {
  it('allow nhận tên tool hoặc *', () => {
    expect(resolveAccess([ref({ allow: ['a'] })], tool('a'))).toBe('allow');
    expect(resolveAccess([ref({ allow: ['a'] })], tool('b'))).toBeNull();
    expect(resolveAccess([ref({})], tool('b'))).toBe('allow');
  });
  it('deny thắng trên mọi mục của cùng server', () => {
    expect(resolveAccess([ref({}), ref({ allow: [], deny: ['a'] })], tool('a'))).toBe('deny');
    expect(resolveAccess([ref({ deny: ['*'] })], tool('a'))).toBe('deny');
  });
  it('require_approval: always của mục đang cho phép làm lời gọi cần duyệt', () => {
    expect(resolveAccess([ref({ require_approval: 'always' })], tool('a'))).toBe('approval');
  });
  it('server khác thì không có quyết định', () => {
    expect(resolveAccess([ref({})], { server_id: 'khac', name: 'a' })).toBeNull();
  });
});

describe('adapter', () => {
  it('maxRisk lấy mức cao nhất; endpointHost chỉ lấy tên máy', () => {
    expect(maxRisk(['read', 'destructive', 'write'])).toBe('destructive');
    expect(maxRisk([])).toBeNull();
    expect(endpointHost('https://mcp.example.com:8443/path?x=1')).toBe('mcp.example.com:8443');
    expect(endpointHost('không phải url')).toBeNull();
    expect(endpointHost(null)).toBeNull();
  });

  it('dựng node và cạnh từ ảnh chụp, đếm theo dữ liệu thật', () => {
    const model = buildGraph(topology({ agents: 4, servers: 2, toolsPerServer: 3 }));
    const kinds = (kind: string) => model.nodes.filter(n => n.kind === kind).length;
    expect([kinds('control'), kinds('agent'), kinds('server'), kinds('tool'), kinds('system')]).toEqual([1, 4, 2, 6, 2]);
    const control = model.nodes[0];
    expect(control).toMatchObject({ kind: 'control', agents: 4, activeAgents: 4, servers: 2, pendingApprovals: 2, pendingReviews: 1 });
    // Agent 0: allow * trên server 0 (deny tool_1, luôn cần duyệt), allow tool_0 trên server 1 (đang tắt).
    const uses = model.edges.filter(e => e.kind === 'uses' && e.source === model.nodes[1]!.id);
    expect(uses.map(e => [e.state, e.allowed, e.denied])).toEqual([['approval', 2, 1], ['broken', 1, 0]]);
    const agent = model.nodes[1] as AgentNode;
    expect(agent).toMatchObject({ servers: 2, tools: 2, risk: 'write' });
  });

  it('Agent không đọc được cấu hình vẫn hiện, không có quan hệ tới server', () => {
    const data = topology({ agents: 1, servers: 1, toolsPerServer: 2 });
    data.agents[0]!.config = null;
    const model = buildGraph(data);
    expect(model.edges.filter(e => e.kind === 'uses')).toEqual([]);
    expect(model.nodes.find(n => n.kind === 'agent')).toMatchObject({ risk: null, servers: 0 });
  });

  it('bỏ qua tham chiếu tới server mà người xem không thấy', () => {
    const data = topology({ agents: 1, servers: 1, toolsPerServer: 1 });
    data.agents[0]!.config!.tools.push({ server_id: 'khong-co', allow: ['*'], require_approval: 'policy' });
    expect(buildGraph(data).edges.every(e => e.target !== 'server:khong-co')).toBe(true);
  });
});

describe('lớp nhìn', () => {
  const model = buildGraph(topology({ agents: 6, servers: 3, toolsPerServer: 4 }));
  const all = new Set(model.nodes.filter(n => n.kind === 'server').map(n => n.id));

  it('server chưa mở thì tool được thu gọn và đường tới hệ thống đích đi từ server', () => {
    const view = applyFilters(model, DEFAULT_FILTERS, new Set());
    expect(view.nodes.some(n => n.kind === 'tool')).toBe(false);
    expect([...view.folded.values()]).toEqual([4, 4, 4]);
    expect(view.edges.filter(e => e.kind === 'connects').every(e => e.source.startsWith('server:'))).toBe(true);
    const open = applyFilters(model, DEFAULT_FILTERS, all);
    expect(open.edges.filter(e => e.kind === 'connects').every(e => e.source.startsWith('tool:'))).toBe(true);
  });

  it('lọc theo môi trường, trạng thái, rủi ro và loại node; cạnh treo bị bỏ', () => {
    const prod = applyFilters(model, { ...DEFAULT_FILTERS, environment: 'prod' }, all);
    expect(prod.nodes.filter(n => n.kind === 'agent').every(n => (n as AgentNode).environment === 'prod')).toBe(true);
    const disabled = applyFilters(model, { ...DEFAULT_FILTERS, status: 'disabled' }, all);
    expect(disabled.nodes.filter(n => n.kind === 'agent').map(n => n.label)).toEqual(['Agent 5']);
    const noServers = applyFilters(model, { ...DEFAULT_FILTERS, hidden: ['server'] }, all);
    const ids = new Set(noServers.nodes.map(n => n.id));
    expect(noServers.nodes.some(n => n.kind === 'server')).toBe(false);
    expect(noServers.edges.every(e => ids.has(e.source) && ids.has(e.target))).toBe(true);
    const risky = applyFilters(model, { ...DEFAULT_FILTERS, risk: 'destructive' }, all);
    expect(risky.nodes.filter(n => n.kind === 'tool').every(n => n.kind === 'tool' && n.risk === 'destructive')).toBe(true);
  });

  it('ẩn node không có quan hệ', () => {
    const data = topology({ agents: 1, servers: 3, toolsPerServer: 1 });
    const lone = buildGraph(data);
    const view = applyFilters(lone, { ...DEFAULT_FILTERS, hideDisconnected: true, projectId: ALL }, defaultExpanded(lone));
    expect(view.nodes.filter(n => n.kind === 'server').map(n => (n as ServerNode).slug)).toEqual(['server-0', 'server-1']);
  });

  it('chọn một Agent: liên quan là server, tool nó chạm tới; tool bị deny được đánh dấu', () => {
    const view = applyFilters(model, DEFAULT_FILTERS, all);
    const agent = model.nodes.find(n => n.kind === 'agent')!;
    const focus = related(model, view, agent.id)!;
    expect(focus.nodes.has(nodeId.control)).toBe(true);
    expect([...focus.nodes].filter(id => id.startsWith('server:'))).toHaveLength(2);
    expect(focus.denied.size).toBe(1);
    expect([...focus.nodes].some(id => id.startsWith('agent:') && id !== agent.id)).toBe(false);
    expect(related(model, view, nodeId.control)).toBeNull();
    expect(related(model, view, null)).toBeNull();
  });

  it('chọn một tool: liên quan là server của nó và các Agent được gọi nó', () => {
    const view = applyFilters(model, DEFAULT_FILTERS, all);
    const shared = model.nodes.find(n => n.kind === 'tool' && n.name === 'tool_0' && n.serverNode === 'server:' + model.nodes.filter(x => x.kind === 'server')[1]!.id.slice(7))!;
    const focus = related(model, view, shared.id)!;
    const agents = [...focus.nodes].filter(id => id.startsWith('agent:'));
    expect(agents).toHaveLength(4);
    for (const id of agents) expect(model.access.get(id)?.get(shared.id)).not.toBe('deny');
    expect(focus.nodes.has(shared.kind === 'tool' ? shared.serverNode : '')).toBe(true);
    // Tool bị một Agent từ chối: Agent đó không nằm trong tập liên quan.
    const denied = model.nodes.find(n => n.kind === 'tool' && n.deniedBy > 0)!;
    expect([...related(model, view, denied.id)!.nodes].filter(id => id.startsWith('agent:'))).toEqual([]);
  });

  it('chọn một cạnh: liên quan là hai đầu của nó', () => {
    const view = applyFilters(model, DEFAULT_FILTERS, all);
    const edge = view.edges.find(e => e.kind === 'uses')!;
    expect([...related(model, view, edge.id)!.nodes].sort()).toEqual([edge.source, edge.target].sort());
  });

  it('lần chạy đang theo dõi: Agent, tool đã gọi và đường nối giữa chúng', () => {
    const view = applyFilters(model, DEFAULT_FILTERS, all);
    const agent = model.nodes.find(n => n.kind === 'agent') as AgentNode;
    const called = model.nodes.find(n => n.kind === 'tool' && model.access.get(agent.id)?.get(n.id) === 'approval')!;
    const toolId = called.kind === 'tool' ? called.toolId : '';
    const live = liveFocus(model, view, [agent.agentId], [toolId]);
    expect(live.nodes.has(agent.id) && live.nodes.has(called.id)).toBe(true);
    expect([...live.edges].map(id => id.split(':')[0]).sort()).toEqual(['exposes', 'manages', 'uses']);
    // Tool đang thu gọn vào server: đường Agent → server vẫn sáng.
    const folded = liveFocus(model, applyFilters(model, DEFAULT_FILTERS, new Set()), [agent.agentId], [toolId]);
    expect([...folded.edges].map(id => id.split(':')[0]).sort()).toEqual(['manages', 'uses']);
  });

  it('tìm node theo tên không dấu, slug hoặc định danh', () => {
    expect(search(model.nodes, 'du an 1').every(n => n.kind === 'agent')).toBe(true);
    expect(search(model.nodes, 'server-2').map(n => n.label)).toEqual(['Server 2']);
    expect(search(model.nodes, '   ')).toEqual([]);
  });
});

describe('bản đồ lớn (khoảng 200 node)', () => {
  const model = buildGraph(topology({ projects: 4, agents: 60, servers: 12, toolsPerServer: 10 }));

  it('mở ra với tool thu gọn; mở hết vẫn bố cục được, không node nào chồng node nào', () => {
    expect(defaultExpanded(model).size).toBe(0);
    const everything = new Set(model.nodes.filter(n => n.kind === 'server').map(n => n.id));
    const view = applyFilters(model, DEFAULT_FILTERS, everything);
    expect(view.nodes.length).toBeGreaterThanOrEqual(190);
    const started = performance.now();
    const at = layout(view);
    expect(performance.now() - started).toBeLessThan(500);
    expect(at.size).toBe(view.nodes.length);
    const columns = new Map<number, { y: number; h: number }[]>();
    for (const n of view.nodes) {
      const p = at.get(n.id)!;
      const list = columns.get(p.x) ?? [];
      list.push({ y: p.y, h: NODE_SIZE[n.kind].height });
      columns.set(p.x, list);
    }
    for (const list of columns.values()) {
      list.sort((a, b) => a.y - b.y);
      for (let i = 1; i < list.length; i++) expect(list[i]!.y).toBeGreaterThanOrEqual(list[i - 1]!.y + list[i - 1]!.h);
    }
  });
});

describe('phần không đọc được', () => {
  it('số lời gọi phụ thất bại đi tới model để bản đồ cảnh báo', () => {
    const data = topology({ agents: 1, servers: 1, toolsPerServer: 1 });
    data.unread = 3;
    expect(buildGraph(data).unread).toBe(3);
  });
});
