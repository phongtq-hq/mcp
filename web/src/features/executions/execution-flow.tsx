import { useEffect, useState } from 'react';
import type { Schemas } from '@/api/client';
import { FlowDiagram, type FlowLabels } from '@/components/flow-diagram';
import { t } from '@/i18n';
import { flowOf, replayFrames, type FlowNode, type FlowStep } from '@/lib/flow';
import { fmtNumber } from '@/lib/format';
import { durationMs } from '@/lib/motion';

type Step = Schemas['Step'];

const toFlow = (steps: Step[]): FlowStep[] => steps.map(s => ({ seq: s.step_seq, kind: s.kind, status: s.status, toolName: s.tool_name, durationMs: s.duration_ms }));

// Số lượt và tổng thời gian của một node, từ `duration_ms` của các bước.
function tally(n: FlowNode, unit: string): string | null {
  if (n.count === 0) return null;
  return `${fmtNumber(n.count)} ${unit} · ${fmtNumber(n.ms)} ms`;
}

// ExecutionFlow: sơ đồ của một lần chạy một Agent. Đang chạy thì nó theo trạng thái thật
// (trang tải lại theo luồng sự kiện). Đã xong thì có nút phát lại: đi qua các bước theo đúng
// thứ tự đã xảy ra, dừng được bất cứ lúc nào, và không chặn thao tác nào khác trên trang.
export function ExecutionFlow({ status, steps, agentName, modelName, live }: { status: string; steps: Step[]; agentName: string; modelName: string | null; live: boolean }) {
  const [frame, setFrame] = useState<number | null>(null);
  const flowSteps = toFlow(steps);
  const frames = live ? [] : replayFrames(status, flowSteps);
  const playing = frame !== null && frame < frames.length;

  useEffect(() => {
    if (frame === null) return;
    if (frame >= frames.length - 1) { const done = setTimeout(() => setFrame(null), durationMs('flow')); return () => clearTimeout(done); }
    const next = setTimeout(() => setFrame(frame + 1), Math.max(durationMs('flow'), 16) * 1.4);
    return () => clearTimeout(next);
  }, [frame, frames.length]);

  const actual = flowOf(status, flowSteps);
  const flow = playing ? frames[frame] ?? actual : actual;
  const labels: FlowLabels = {
    title: t('flowTitle'),
    node: { user: t('flowUser'), agent: agentName, model: modelName ?? t('flowModel'), tool: t('flowTool'), result: t('flowResult') },
    detail: {
      model: tally(actual.nodes.model, t('flowCalls')),
      tool: actual.nodes.tool.count > 0
        ? <>{tally(actual.nodes.tool, t('flowCalls'))}{actual.nodes.tool.tools.slice(0, 2).map(name => <div key={name} className="mono">{name}</div>)}</>
        : null,
    },
    state: { idle: t('flowIdle'), active: t('flowActive'), waiting: t('flowWaiting'), done: t('flowDone'), error: t('flowError') },
  };
  const canReplay = !live && frames.length > 2;
  return (
    <FlowDiagram
      flow={flow}
      labels={labels}
      actions={canReplay ? <button type="button" onClick={() => setFrame(playing ? null : 0)}>{playing ? t('flowStop') : t('flowReplay')}</button> : null}
    />
  );
}
