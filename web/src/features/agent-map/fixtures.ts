// Ảnh chụp tổng hợp cho TEST (adapter, lọc, bố cục với 50–200 node). Không màn hình nào
// import file này: bản đồ trong sản phẩm chỉ vẽ dữ liệu thật từ source.ts. Dữ liệu sinh theo
// công thức cố định (không ngẫu nhiên) để test ổn định.
import type { AgentConfig, AgentEntry, McpServer, McpTool, Project, RiskLevel, Topology } from './model';

const RISK: readonly RiskLevel[] = ['read', 'read', 'write', 'destructive'];
const uuid = (group: number, n: number) => `00000000-0000-4000-8${String(group).padStart(3, '0')}-${String(n).padStart(12, '0')}`;

export interface FixtureSize { projects?: number; agents: number; servers: number; toolsPerServer: number }

export function topology({ projects = 2, agents, servers, toolsPerServer }: FixtureSize): Topology {
  const projectList: Project[] = Array.from({ length: projects }, (_, i) => ({
    id: uuid(1, i), slug: `project-${i}`, name: `Dự án ${i}`, environment: i % 2 === 0 ? 'non_prod' : 'prod', status: 'active', settings: {},
  } as unknown as Project));
  const serverList: McpServer[] = Array.from({ length: servers }, (_, i) => ({
    id: uuid(2, i), slug: `server-${i}`, name: `Server ${i}`, scope: 'organization', kind: i % 3 === 0 ? 'external' : 'managed', transport: 'streamable_http',
    endpoint: `https://host-${i % 4}.example.internal:8443/mcp`, status: i === servers - 1 && servers > 1 ? 'disabled' : 'active',
    health_status: i % 5 === 4 ? 'degraded' : 'healthy', active_version: 1, pending_review_version: i === 0 ? 2 : null,
  }));
  const tools: McpTool[] = serverList.flatMap((server, s) => Array.from({ length: toolsPerServer }, (_, i): McpTool => ({
    id: uuid(3, s * 1000 + i), server_id: server.id, name: `tool_${i}`, description: `Tool ${i} của ${server.slug}`, input_schema: { type: 'object' },
    enabled: i % 7 !== 6, risk_level: RISK[i % RISK.length] ?? 'read', side_effect: i % 4 < 2 ? 'none' : 'non_idempotent',
  })));
  const entries: AgentEntry[] = Array.from({ length: agents }, (_, i) => {
    const project = projectList[i % projects]!;
    const first = serverList[i % servers]!;
    const second = serverList[(i + 1) % servers]!;
    const config: AgentConfig = {
      model: { policy: 'tiered', default_tier: 'standard' },
      limits: { max_iterations: 8, max_tool_calls: 16, timeout_seconds: 300 },
      tools: [
        { server_id: first.id, allow: ['*'], deny: i % 3 === 0 ? ['tool_1'] : [], require_approval: i % 4 === 0 ? 'always' : 'policy' },
        ...(servers > 1 ? [{ server_id: second.id, allow: ['tool_0'], require_approval: 'policy' as const }] : []),
      ],
      ...(i > 0 && i % 5 === 0 ? { delegation: { can_delegate_to: [uuid(4, i - 1)], max_depth: 2 } } : {}),
    };
    return {
      agent: { id: uuid(4, i), project_id: project.id, slug: `agent-${i}`, name: `Agent ${i}`, description: '', status: i % 6 === 5 ? 'disabled' : 'active', active_version: 1, latest_version: 1 },
      project, config, version: 1,
    };
  });
  return { organization: 'Tổ chức thử', agents: entries, servers: serverList, tools, pendingApprovals: 2, truncated: false, unread: 0 };
}
