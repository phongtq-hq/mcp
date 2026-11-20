// Mô hình của bản đồ Agent. Ba lớp tách nhau:
//   Topology   = ảnh chụp các thực thể của API (source.ts đọc; fixtures.ts dựng cho test);
//   GraphModel = đồ thị nghiệp vụ suy ra từ ảnh chụp đó (adapter.ts), không biết gì về vẽ;
//   khung vẽ   = node / cạnh của thư viện đồ thị kèm toạ độ (view.ts + lib/graph-layout.ts).
// Component vẽ chỉ nhận GraphModel: nó không biết dữ liệu đến từ API hay từ fixture.
import type { Schemas } from '@/api/client';

export type Agent = Schemas['Agent'];
export type AgentConfig = Schemas['AgentConfig'];
export type Project = Schemas['Project'];
export type McpServer = Schemas['McpServer'];
export type McpTool = Schemas['McpTool'];
export type RiskLevel = Schemas['RiskLevel'];

export interface AgentEntry {
  agent: Agent;
  project: Project;
  // Cấu hình của version đang hoạt động (hoặc version mới nhất khi chưa publish). null khi
  // không đọc được (thiếu quyền, lỗi): Agent vẫn hiện, chỉ không có quan hệ tới tool.
  config: AgentConfig | null;
  version: number | null;
}

export interface Topology {
  organization: string;
  agents: AgentEntry[];
  servers: McpServer[];
  tools: McpTool[];
  // null: không có quyền đọc approval.
  pendingApprovals: number | null;
  // Danh sách bị cắt ở trần của trang API: bản đồ nói rõ là chưa đủ.
  truncated: boolean;
  // Số lời gọi phụ không đọc được (thiếu quyền, lỗi mạng): phần tương ứng VẮNG MẶT trên bản
  // đồ, khác với "không có". Bản đồ phải nói rõ điều đó.
  unread: number;
}

export type NodeKind = 'control' | 'agent' | 'server' | 'tool' | 'system';
export type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'idle';

export const KINDS: readonly NodeKind[] = ['control', 'agent', 'server', 'tool', 'system'];
export const RISKS: readonly RiskLevel[] = ['read', 'write', 'destructive', 'financial'];

interface Base { id: string; label: string; tone: Tone }

export interface ControlNode extends Base {
  kind: 'control';
  agents: number;
  activeAgents: number;
  servers: number;
  healthyServers: number;
  tools: number;
  // Tool đang tắt (kể cả bị khoá vì định nghĩa đổi) và server có version chờ duyệt.
  disabledTools: number;
  pendingReviews: number;
  pendingApprovals: number | null;
}

export interface AgentNode extends Base {
  kind: 'agent';
  agentId: string;
  slug: string;
  description: string;
  projectId: string;
  projectName: string;
  environment: string;
  status: string;
  version: number | null;
  published: boolean;
  // Mức rủi ro cao nhất trong các tool đang bật mà Agent được gọi; null khi không có tool nào.
  risk: RiskLevel | null;
  servers: number;
  tools: number;
  config: AgentConfig | null;
}

export interface ServerNode extends Base {
  kind: 'server';
  serverId: string;
  slug: string;
  serverKind: string;
  scope: string;
  transport: string;
  status: string;
  health: string;
  version: number | null;
  pendingReview: boolean;
  tools: number;
  enabledTools: number;
  agents: number;
  systemId: string | null;
}

export interface ToolNode extends Base {
  kind: 'tool';
  toolId: string;
  serverId: string;
  serverNode: string;
  name: string;
  description: string;
  risk: RiskLevel;
  sideEffect: string;
  enabled: boolean;
  allowedBy: number;
  deniedBy: number;
  inputSchema: unknown;
}

export interface SystemNode extends Base {
  kind: 'system';
  host: string;
  servers: number;
}

export type GraphNode = ControlNode | AgentNode | ServerNode | ToolNode | SystemNode;

export type EdgeKind = 'manages' | 'uses' | 'exposes' | 'delegates' | 'connects';
// ok: quan hệ dùng được. approval: dùng được nhưng mọi lời gọi cần người duyệt.
// denied: cấu hình của Agent từ chối. broken: đầu kia không dùng được (server chưa hoạt
// động / không tới được, tool đang tắt, Agent đích không còn hoạt động).
export type EdgeState = 'ok' | 'approval' | 'denied' | 'broken';

export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  kind: EdgeKind;
  state: EdgeState;
  // Chỉ cho cạnh Agent → server: số tool được gọi / bị từ chối trên server đó.
  allowed?: number;
  denied?: number;
}

export type Access = 'allow' | 'approval' | 'deny';

export interface GraphModel {
  nodes: GraphNode[];
  edges: GraphEdge[];
  // access[node Agent][node tool]: quyết định của cấu hình Agent với từng tool. Tool không có
  // mặt nghĩa là Agent không tham chiếu tới nó.
  access: Map<string, Map<string, Access>>;
  truncated: boolean;
  unread: number;
}

export const nodeId = {
  control: 'control',
  agent: (id: string) => 'agent:' + id,
  server: (id: string) => 'server:' + id,
  tool: (id: string) => 'tool:' + id,
  system: (host: string) => 'system:' + host,
};
