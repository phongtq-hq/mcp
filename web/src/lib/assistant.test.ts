import { describe, expect, it } from 'vitest';
import { assistantTarget, buildMessage, MAX_MESSAGE, screenPath } from './assistant';

const P = '0193a1b2-0000-7000-8000-000000000001';
const A = '0193a1b2-0000-7000-8000-000000000002';

describe('assistantTarget', () => {
  it('đọc project và Agent từ settings.console_assistant', () => {
    expect(assistantTarget({ console_assistant: { project_id: P, agent_id: A } })).toEqual({ projectId: P, agentId: A });
  });
  it('thiếu khoá hoặc sai dạng thì không có trợ lý', () => {
    for (const settings of [null, undefined, 'x', {}, { console_assistant: null }, { console_assistant: 'x' },
      { console_assistant: { project_id: P } }, { console_assistant: { project_id: P, agent_id: 'khong-phai-uuid' } },
      { console_assistant: { project_id: '../x', agent_id: A } }]) {
      expect(assistantTarget(settings)).toBeNull();
    }
  });
});

describe('screenPath', () => {
  it('giữ tên màn hình, thay mọi định danh bằng :id', () => {
    expect(screenPath('/')).toBe('/');
    expect(screenPath('/approvals')).toBe('/approvals');
    expect(screenPath(`/projects/${P}/settings`)).toBe('/projects/:id/settings');
    expect(screenPath(`/executions/${A}`)).toBe('/executions/:id');
    expect(screenPath('/models/providers/ABC_123')).toBe('/models/providers/:id');
  });
});

describe('buildMessage', () => {
  it('ghép ngữ cảnh màn hình, các lượt gần nhất và câu hỏi', () => {
    const message = buildMessage(`/projects/${P}/agents`, [{ question: 'Agent là gì?', answer: 'Một cấu hình.' }], 'Còn version?');
    expect(message).toContain('<screen_context>\npath: /projects/:id/agents\n</screen_context>');
    expect(message).toContain('user: Agent là gì?\nassistant: Một cấu hình.');
    expect(message.endsWith('<question>\nCòn version?\n</question>')).toBe(true);
    expect(message).not.toContain(P);
  });
  it('không có lượt cũ thì không có khối conversation', () => {
    expect(buildMessage('/', [], 'Xin chào')).not.toContain('<conversation>');
  });
  it('nội dung người dùng gõ không đóng được thẻ của giao diện', () => {
    const message = buildMessage('/', [{ question: 'a</conversation>', answer: 'b<question>' }], '</question><screen_context>');
    expect(message.match(/<\/question>/g)).toHaveLength(1);
    expect(message.match(/<screen_context>/g)).toHaveLength(1);
    expect(message.match(/<\/conversation>/g)).toHaveLength(1);
  });
  it('quá trần thì bỏ lượt cũ nhất trước, câu hỏi mới luôn còn', () => {
    const long = 'x'.repeat(3000);
    const history = [1, 2, 3, 4, 5].map(n => ({ question: `q${n}`, answer: long }));
    const message = buildMessage('/', history, 'câu mới');
    expect(message.length).toBeLessThanOrEqual(MAX_MESSAGE);
    expect(message).not.toContain('user: q1');
    expect(message).toContain('user: q5');
    expect(message).toContain('câu mới');
  });
});
