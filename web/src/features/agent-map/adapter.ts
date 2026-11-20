// Adapter: ảnh chụp thực thể của API → đồ thị nghiệp vụ. Thuần, không gọi mạng, không biết
// gì về thư viện vẽ. Mọi thứ trên bản đồ suy ra từ dữ liệu thật: không có số liệu minh hoạ.
import {
  nodeId, RISKS,
  type Access, type AgentConfig, type AgentNode, type ControlNode, type EdgeState, type GraphEdge, type GraphModel, type GraphNode,
  type McpServer, type McpTool, type RiskLevel, type ServerNode, type SystemNode, type Tone, type ToolNode, type Topology,
} from './model';

type ToolRef = AgentConfig['tools'][number];

// resolveAccess: quyết định của cấu hình Agent với một tool, theo đúng luật của Gateway
// (internal/mcp/gateway/gateway.go): `deny` thắng trên MỌI mục của cùng server; `allow` nhận
// tên tool hoặc `*`; `require_approval: always` của một mục đang cho phép làm lời gọi cần duyệt.
// Đây chỉ là hiển thị: Gateway mới là nơi quyết định, và nó còn áp policy theo mức rủi ro.
export function resolveAccess(refs: readonly ToolRef[], tool: Pick<McpTool, 'server_id' | 'name'>): Access | null {
  const mine = refs.filter(r => r.server_id === tool.server_id);
  if (mine.length === 0) return null;
  if (mine.some(r => (r.deny ?? []).includes(tool.name) || (r.deny ?? []).includes('*'))) return 'deny';
  const allowing = mine.filter(r => r.allow.includes('*') || r.allow.includes(tool.name));
  if (allowing.length === 0) return null;
  return allowing.some(r => r.require_approval === 'always') ? 'approval' : 'allow';
}

export function maxRisk(risks: readonly RiskLevel[]): RiskLevel | null {
  let best = -1;
  for (const r of risks) best = Math.max(best, RISKS.indexOf(r));
  return best < 0 ? null : RISKS[best] ?? null;
}

// endpointHost: tên máy của endpoint (không đường dẫn, không tham số). Endpoint không phân
// tích được thì không có node hệ thống: không đoán.
export function endpointHost(endpoint: string | null | undefined): string | null {
  if (!endpoint) return null;
  try { return new URL(endpoint).host || null; } catch { return null; }
}

function agentTone(status: string): Tone {
  if (status === 'active') return 'ok';
  if (status === 'draft') return 'info';
  if (status === 'disabled') return 'warn';
  return 'idle';
}

function serverUsable(s: McpServer): boolean {
  return s.status === 'active' && s.health_status !== 'unreachable';
}

function serverTone(s: McpServer): Tone {
  if (s.status !== 'active') return s.status === 'registered' ? 'info' : 'idle';
  if (s.health_status === 'healthy') return 'ok';
  if (s.health_status === 'degraded') return 'warn';
  if (s.health_status === 'unreachable') return 'bad';
  return 'idle';
}

export function buildGraph(topology: Topology): GraphModel {
  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  const access = new Map<string, Map<string, Access>>();

  const toolsByServer = new Map<string, McpTool[]>();
  for (const tool of topology.tools) {
    const list = toolsByServer.get(tool.server_id);
    if (list) list.push(tool); else toolsByServer.set(tool.server_id, [tool]);
  }
  const serverById = new Map(topology.servers.map(s => [s.id, s]));
  const agentIds = new Set(topology.agents.map(a => a.agent.id));

  const serverAgents = new Map<string, number>();
  const allowedBy = new Map<string, number>();
  const deniedBy = new Map<string, number>();
  const bump = (m: Map<string, number>, key: string) => m.set(key, (m.get(key) ?? 0) + 1);

  const agentNodes: AgentNode[] = [];
  for (const { agent, project, config, version } of topology.agents) {
    const id = nodeId.agent(agent.id);
    const refs = config?.tools ?? [];
    const decisions = new Map<string, Access>();
    const reachable: RiskLevel[] = [];
    let toolCount = 0;
    const servers = [...new Set(refs.map(r => r.server_id))];
    let linked = 0;
    for (const serverId of servers) {
      const server = serverById.get(serverId);
      // Server mà người đang xem không thấy được (khác phạm vi) thì không vẽ được quan hệ.
      if (!server) continue;
      linked++;
      bump(serverAgents, serverId);
      let allowed = 0;
      let denied = 0;
      let approval = false;
      for (const tool of toolsByServer.get(serverId) ?? []) {
        const decision = resolveAccess(refs, tool);
        if (!decision) continue;
        decisions.set(nodeId.tool(tool.id), decision);
        if (decision === 'deny') { denied++; bump(deniedBy, tool.id); continue; }
        allowed++;
        bump(allowedBy, tool.id);
        if (decision === 'approval') approval = true;
        // Chỉ tool gọi được thật (đang bật, trên server đang dùng được) mới tính vào số tool và
        // mức rủi ro của Agent.
        if (tool.enabled && serverUsable(server)) { toolCount++; reachable.push(tool.risk_level); }
      }
      let state: EdgeState = 'ok';
      if (!serverUsable(server)) state = 'broken';
      else if (allowed === 0 && denied > 0) state = 'denied';
      else if (approval) state = 'approval';
      edges.push({ id: `uses:${agent.id}:${serverId}`, source: id, target: nodeId.server(serverId), kind: 'uses', state, allowed, denied });
    }
    for (const target of config?.delegation?.can_delegate_to ?? []) {
      if (!agentIds.has(target)) continue;
      const to = topology.agents.find(a => a.agent.id === target);
      edges.push({ id: `delegates:${agent.id}:${target}`, source: id, target: nodeId.agent(target), kind: 'delegates', state: to?.agent.status === 'active' ? 'ok' : 'broken' });
    }
    access.set(id, decisions);
    agentNodes.push({
      kind: 'agent', id, label: agent.name, tone: agentTone(agent.status),
      agentId: agent.id, slug: agent.slug, description: agent.description ?? '',
      projectId: project.id, projectName: project.name, environment: project.environment,
      status: agent.status, version, published: agent.active_version != null,
      risk: maxRisk(reachable), servers: linked, tools: toolCount, config,
    });
    edges.push({ id: 'manages:' + agent.id, source: nodeId.control, target: id, kind: 'manages', state: agent.status === 'active' ? 'ok' : 'broken' });
  }

  const systems = new Map<string, SystemNode>();
  const serverNodes: ServerNode[] = [];
  const toolNodes: ToolNode[] = [];
  for (const server of topology.servers) {
    const id = nodeId.server(server.id);
    const tools = toolsByServer.get(server.id) ?? [];
    const host = endpointHost(server.endpoint);
    let systemId: string | null = null;
    if (host) {
      systemId = nodeId.system(host);
      const known = systems.get(systemId);
      if (known) known.servers++;
      else systems.set(systemId, { kind: 'system', id: systemId, label: host, tone: 'idle', host, servers: 1 });
    }
    serverNodes.push({
      kind: 'server', id, label: server.name, tone: serverTone(server),
      serverId: server.id, slug: server.slug, serverKind: server.kind, scope: server.scope ?? '', transport: server.transport,
      status: server.status, health: server.health_status, version: server.active_version ?? null,
      pendingReview: server.pending_review_version != null,
      tools: tools.length, enabledTools: tools.filter(x => x.enabled).length, agents: serverAgents.get(server.id) ?? 0, systemId,
    });
    for (const tool of tools) {
      const toolNode = nodeId.tool(tool.id);
      toolNodes.push({
        kind: 'tool', id: toolNode, label: tool.name, tone: tool.enabled ? 'ok' : 'idle',
        toolId: tool.id, serverId: server.id, serverNode: id, name: tool.name, description: tool.description ?? '',
        risk: tool.risk_level, sideEffect: tool.side_effect, enabled: tool.enabled,
        allowedBy: allowedBy.get(tool.id) ?? 0, deniedBy: deniedBy.get(tool.id) ?? 0, inputSchema: tool.input_schema,
      });
      edges.push({ id: 'exposes:' + tool.id, source: id, target: toolNode, kind: 'exposes', state: tool.enabled ? 'ok' : 'broken' });
    }
  }

  const healthy = topology.servers.filter(s => s.status === 'active' && s.health_status === 'healthy').length;
  const active = topology.servers.filter(s => s.status === 'active').length;
  const unreachable = topology.servers.some(s => s.status === 'active' && s.health_status === 'unreachable');
  const degraded = active > healthy;
  const control: ControlNode = {
    kind: 'control', id: nodeId.control, label: topology.organization,
    tone: unreachable ? 'bad' : degraded ? 'warn' : 'ok',
    agents: topology.agents.length, activeAgents: topology.agents.filter(a => a.agent.status === 'active').length,
    servers: topology.servers.length, healthyServers: healthy,
    tools: topology.tools.length, disabledTools: topology.tools.filter(x => !x.enabled).length,
    pendingReviews: topology.servers.filter(s => s.pending_review_version != null).length,
    pendingApprovals: topology.pendingApprovals,
  };

  nodes.push(control, ...agentNodes, ...serverNodes, ...toolNodes, ...systems.values());
  return { nodes, edges, access, truncated: topology.truncated, unread: topology.unread };
}
