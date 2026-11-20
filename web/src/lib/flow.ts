// Luồng của một lần chạy, suy ra từ các bước THẬT của nó (execution_steps): người dùng →
// Agent → model / tool → kết quả. Không có dữ liệu nào được dựng thêm ở đây.
//
// Giới hạn của dữ liệu hiện có (xem báo cáo backend): một bước `tool_call` gộp cả chặng
// Gateway lẫn tool, nên hai thứ đó là MỘT node; và sự kiện tới theo nhịp bước, không theo
// token, nên sơ đồ chuyển theo từng bước.
export type FlowNodeId = 'user' | 'agent' | 'model' | 'tool' | 'result';
export type FlowState = 'idle' | 'active' | 'waiting' | 'done' | 'error';
export type FlowEdgeId = 'user-agent' | 'agent-model' | 'agent-tool' | 'agent-result';

export interface FlowStep { seq: number; kind: string; status: string; toolName?: string | null; durationMs?: number | null }
export interface FlowNode { state: FlowState; count: number; ms: number; tools: string[] }
export interface Flow { nodes: Record<FlowNodeId, FlowNode>; active: FlowEdgeId | null }

const TERMINAL_OK = 'completed';
const TERMINAL_BAD: ReadonlySet<string> = new Set(['failed', 'cancelled', 'budget_exceeded', 'timed_out']);
const STEP_BAD: ReadonlySet<string> = new Set(['failed', 'denied', 'unknown']);

const node = (state: FlowState = 'idle'): FlowNode => ({ state, count: 0, ms: 0, tools: [] });

export function flowOf(executionStatus: string, steps: readonly FlowStep[]): Flow {
  const nodes: Record<FlowNodeId, FlowNode> = { user: node('done'), agent: node(), model: node(), tool: node(), result: node() };
  const ordered = [...steps].sort((a, b) => a.seq - b.seq);
  for (const s of ordered) {
    const target = s.kind === 'llm_call' ? nodes.model : s.kind === 'tool_call' || s.kind === 'approval_wait' ? nodes.tool : null;
    if (!target) continue;
    if (s.kind !== 'approval_wait') target.count++;
    target.ms += s.durationMs ?? 0;
    if (s.toolName && !target.tools.includes(s.toolName)) target.tools.push(s.toolName);
    if (STEP_BAD.has(s.status)) target.state = 'error';
    else if (target.state !== 'error') target.state = 'done';
  }

  const done = executionStatus === TERMINAL_OK;
  const bad = TERMINAL_BAD.has(executionStatus);
  if (done || bad) {
    nodes.agent.state = 'done';
    nodes.result.state = done ? 'done' : 'error';
    return { nodes, active: null };
  }

  // Đang chạy: node đang xử lý là node của bước cuối cùng còn mở; không có bước nào mở thì
  // Agent đang giữ lượt (đang chuẩn bị bước kế tiếp).
  const last = ordered.at(-1);
  const open = last && last.status === 'started' ? last : null;
  if (executionStatus === 'waiting_approval' || open?.kind === 'approval_wait') {
    nodes.agent.state = 'done';
    nodes.tool.state = 'waiting';
    return { nodes, active: null };
  }
  if (executionStatus === 'paused') {
    nodes.agent.state = 'waiting';
    return { nodes, active: null };
  }
  if (open?.kind === 'llm_call') { nodes.agent.state = 'done'; nodes.model.state = 'active'; return { nodes, active: 'agent-model' }; }
  if (open?.kind === 'tool_call') { nodes.agent.state = 'done'; nodes.tool.state = 'active'; return { nodes, active: 'agent-tool' }; }
  nodes.agent.state = 'active';
  return { nodes, active: ordered.length === 0 ? 'user-agent' : null };
}

// replayFrames: các khung để phát lại một lần chạy ĐÃ XONG theo đúng thứ tự bước của nó:
// khung thứ i là sơ đồ lúc bước i đang chạy; khung cuối là trạng thái kết thúc.
export function replayFrames(executionStatus: string, steps: readonly FlowStep[]): Flow[] {
  const ordered = [...steps].sort((a, b) => a.seq - b.seq).filter(s => s.kind === 'llm_call' || s.kind === 'tool_call');
  const frames = ordered.map((s, i) => flowOf('running', [...ordered.slice(0, i), { ...s, status: 'started', durationMs: 0 }]));
  return [flowOf('running', []), ...frames, flowOf(executionStatus, steps)];
}
