// Nguồn dữ liệu thật của bản đồ: đọc các thực thể qua Management API rồi trả một Topology.
// API chưa có endpoint "topology" nên ảnh chụp được ghép từ nhiều lời gọi (dự án → Agent →
// version đang hoạt động), với số lời gọi đồng thời có trần. Giới hạn này được ghi ở
// .docs/ui-refactor-plan.md; cách sửa đúng là một endpoint ở /v1, không phải đường tắt ở đây.
import { get, soft, type Page, type Schemas } from '@/api/client';
import { isUuid } from '@/lib/format';
import type { Agent, AgentEntry, McpServer, McpTool, Project, Topology } from './model';

type Execution = Schemas['Execution'];
type Task = Schemas['Task'];
type Step = Schemas['Step'];

const PAGE = 200;
const CONCURRENCY = 6;
// Trần số Agent trên một bản đồ: quá ngần này thì bản đồ báo là bị cắt.
export const MAX_AGENTS = 200;
const MAX_LIVE_PROJECTS = 12;
const MAX_LIVE_TASKS = 8;

async function pool<T, R>(items: readonly T[], work: (item: T) => Promise<R>): Promise<R[]> {
  const out = new Array<R>(items.length);
  let next = 0;
  const worker = async () => {
    for (let i = next++; i < items.length; i = next++) out[i] = await work(items[i]!);
  };
  await Promise.all(Array.from({ length: Math.min(CONCURRENCY, items.length) }, worker));
  return out;
}

export async function loadTopology(): Promise<Topology> {
  const [organization, projects, servers, tools, pending] = await Promise.all([
    get<Schemas['Organization']>('/organization'),
    get<Page<Project>>(`/projects?limit=${PAGE}`),
    get<Page<McpServer>>(`/mcp-servers?limit=${PAGE}`),
    // Lời gọi phụ: thiếu quyền thì phần đó vắng mặt trên bản đồ, không hỏng cả màn hình.
    soft(get<Page<McpTool>>(`/mcp-tools?limit=${PAGE}`)),
    soft(get<Page<Schemas['Approval']>>('/approvals?status=pending&limit=50')),
  ]);
  let truncated = !!projects.next_cursor || !!servers.next_cursor || !!tools?.next_cursor;
  // Lời gọi phụ thất bại không được lẫn với "không có gì": đếm lại để bản đồ cảnh báo.
  let unread = tools ? 0 : 1;

  // Định danh ghép vào đường dẫn được kiểm dạng như ở mọi màn hình khác.
  const known = projects.data.filter(p => isUuid(p.id));
  const lists = await pool(known, p => soft(get<Page<Agent>>(`/projects/${p.id}/agents?limit=${PAGE}`)));
  let found: { agent: Agent; project: Project }[] = [];
  lists.forEach((list, i) => {
    const project = known[i];
    if (!project) return;
    if (!list) { unread++; return; }
    if (list.next_cursor) truncated = true;
    for (const agent of list.data) if (isUuid(agent.id)) found.push({ agent, project });
  });
  if (found.length > MAX_AGENTS) { found = found.slice(0, MAX_AGENTS); truncated = true; }

  const agents = await pool(found, async ({ agent, project }): Promise<AgentEntry> => {
    const version = agent.active_version ?? agent.latest_version ?? null;
    const detail = version ? await soft(get<Schemas['AgentVersion']>(`/agents/${agent.id}/versions/${version}`)) : null;
    if (version && !detail) unread++;
    return { agent, project, config: detail?.config ?? null, version };
  });

  return {
    organization: organization.name,
    agents,
    servers: servers.data,
    tools: tools?.data ?? [],
    pendingApprovals: pending ? pending.data.length : null,
    truncated,
    unread,
  };
}

// loadRunning: các lần chạy đang diễn ra ở những dự án có Agent trên bản đồ.
export async function loadRunning(projectIds: readonly string[]): Promise<Execution[]> {
  const lists = await pool(projectIds.filter(isUuid).slice(0, MAX_LIVE_PROJECTS), id => soft(get<Page<Execution>>(`/projects/${id}/executions?status=running&limit=20`)));
  return lists.flatMap(list => list?.data ?? []);
}

export interface Activity { agentIds: string[]; toolIds: string[] }

// loadActivity: Agent của một lần chạy và những tool nó đã gọi (từ các bước thật của task).
export async function loadActivity(executionId: string): Promise<Activity> {
  if (!isUuid(executionId)) return { agentIds: [], toolIds: [] };
  const tasks = await get<Page<Task>>(`/executions/${executionId}/tasks?limit=100`);
  const steps = await pool(tasks.data.filter(task => isUuid(task.id)).slice(0, MAX_LIVE_TASKS), task => soft(get<Page<Step>>(`/tasks/${task.id}/steps?limit=200`)));
  const toolIds = new Set<string>();
  for (const list of steps) for (const step of list?.data ?? []) if (step.tool_id) toolIds.add(step.tool_id);
  return { agentIds: [...new Set(tasks.data.map(task => task.agent_id))], toolIds: [...toolIds] };
}
