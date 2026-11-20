import { describe, expect, it } from 'vitest';
import { flowOf, replayFrames, type FlowStep } from './flow';

const llm = (seq: number, status = 'succeeded', durationMs = 1000): FlowStep => ({ seq, kind: 'llm_call', status, durationMs });
const tool = (seq: number, status = 'succeeded', durationMs = 200): FlowStep => ({ seq, kind: 'tool_call', status, durationMs, toolName: 'docker.list_containers' });
const states = (f: ReturnType<typeof flowOf>) => Object.fromEntries(Object.entries(f.nodes).map(([k, v]) => [k, v.state]));

describe('flowOf', () => {
  it('vừa xếp hàng, chưa có bước nào: yêu cầu đang đi tới Agent', () => {
    const f = flowOf('queued', []);
    expect(states(f)).toEqual({ user: 'done', agent: 'active', model: 'idle', tool: 'idle', result: 'idle' });
    expect(f.active).toBe('user-agent');
  });
  it('bước cuối còn mở quyết định node đang xử lý', () => {
    expect(flowOf('running', [llm(1, 'started')]).active).toBe('agent-model');
    const f = flowOf('running', [llm(1), tool(2, 'started')]);
    expect(f.active).toBe('agent-tool');
    expect(states(f)).toMatchObject({ model: 'done', tool: 'active', result: 'idle' });
  });
  it('giữa hai bước: Agent giữ lượt, không đường nối nào đang chạy', () => {
    const f = flowOf('running', [llm(1), tool(2)]);
    expect(states(f)).toMatchObject({ agent: 'active', model: 'done', tool: 'done' });
    expect(f.active).toBeNull();
  });
  it('chờ duyệt: node tool ở trạng thái chờ', () => {
    expect(states(flowOf('waiting_approval', [llm(1), { seq: 2, kind: 'approval_wait', status: 'started' }]))).toMatchObject({ tool: 'waiting', result: 'idle' });
  });
  it('xong: đếm số lượt và cộng thời gian từng node theo bước thật', () => {
    const f = flowOf('completed', [tool(2), llm(3, 'succeeded', 3800), llm(1, 'succeeded', 1200)]);
    expect(f.nodes.model).toMatchObject({ state: 'done', count: 2, ms: 5000 });
    expect(f.nodes.tool).toMatchObject({ state: 'done', count: 1, ms: 200, tools: ['docker.list_containers'] });
    expect(f.nodes.result.state).toBe('done');
    expect(f.active).toBeNull();
  });
  it('bước lỗi đánh dấu đúng node; lần chạy thất bại thì kết quả là lỗi', () => {
    const f = flowOf('failed', [llm(1), tool(2, 'unknown')]);
    expect(states(f)).toMatchObject({ model: 'done', tool: 'error', result: 'error' });
  });
});

describe('replayFrames', () => {
  it('đi qua các bước theo đúng thứ tự, kết thúc ở trạng thái thật', () => {
    const frames = replayFrames('completed', [llm(1), tool(2), llm(3)]);
    expect(frames.map(f => f.active)).toEqual(['user-agent', 'agent-model', 'agent-tool', 'agent-model', null]);
    expect(frames.at(-1)!.nodes.result.state).toBe('done');
  });
});
