import { describe, expect, it } from 'vitest';
import { incomplete, subjectText, toolName } from './approval';

describe('incomplete', () => {
  it('tool call đủ tên tool và tham số thì duyệt được', () => {
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: 'docker.restart_container', arguments: { id: 'web' } } })).toBe(false);
    // Tham số rỗng vẫn là tham số đã hiện.
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: 'docker.prune', arguments: {} } })).toBe(false);
  });

  it('tool call thiếu tên tool hoặc tham số thì không duyệt được', () => {
    expect(incomplete({ subject_type: 'tool_call', payload: { arguments: { id: 'web' } } })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: '', arguments: {} } })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: 42, arguments: {} } })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: 'docker.prune' } })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call', payload: { tool_name: 'docker.prune', arguments: null } })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call', payload: null })).toBe(true);
    expect(incomplete({ subject_type: 'tool_call' })).toBe(true);
  });

  it('cổng workflow cần thông điệp không rỗng', () => {
    expect(incomplete({ subject_type: 'workflow_step', payload: { message: 'Triển khai bản 1.2?' } })).toBe(false);
    expect(incomplete({ subject_type: 'workflow_step', payload: { message: '   ' } })).toBe(true);
    expect(incomplete({ subject_type: 'workflow_step', payload: { message: { text: 'x' } } })).toBe(true);
    expect(incomplete({ subject_type: 'workflow_step', payload: {} })).toBe(true);
  });

  it('loại chủ thể khác không bị khoá ở đây', () => {
    expect(incomplete({ subject_type: 'remediation_plan', payload: {} })).toBe(false);
  });
});

describe('subjectText', () => {
  it('tool call hiện tên tool', () => {
    const a = { subject_type: 'tool_call', payload: { tool_name: 'github.merge_pull_request', arguments: {} } };
    expect(toolName(a)).toBe('github.merge_pull_request');
    expect(subjectText(a)).toBe('github.merge_pull_request');
  });

  it('thông điệp của cổng workflow đi qua visible', () => {
    const rlo = String.fromCharCode(0x202e);
    expect(subjectText({ subject_type: 'workflow_step', payload: { message: 'ok' + rlo } })).toBe('ok\\u202E');
  });
});
