import type { ReactNode } from 'react';
import type { Flow, FlowEdgeId, FlowNodeId, FlowState } from '@/lib/flow';
import { StatusIcon } from './ui';

// Trạng thái của node hiện bằng hình (StatusIcon) lẫn chữ, không chỉ bằng màu viền.
const TONE: Record<FlowState, string | undefined> = { idle: undefined, active: 'info', waiting: 'warn', done: 'ok', error: 'bad' };

export interface FlowLabels {
  title: string;
  node: Record<FlowNodeId, string>;
  detail: Partial<Record<FlowNodeId, ReactNode>>;
  state: Record<FlowState, string>;
}

function Node({ id, flow, labels }: { id: FlowNodeId; flow: Flow; labels: FlowLabels }) {
  const n = flow.nodes[id];
  return (
    <div className="flow-node" data-state={n.state}>
      <div className="flow-name">{labels.node[id]}</div>
      <div className="flow-state"><StatusIcon tone={TONE[n.state]} busy={n.state === 'active'} />{labels.state[n.state]}</div>
      {n.state !== 'idle' && labels.detail[id] ? <div className="flow-detail">{labels.detail[id]}</div> : null}
    </div>
  );
}

function Edge({ id, flow, passed }: { id: FlowEdgeId; flow: Flow; passed: boolean }) {
  return <div className="flow-edge" aria-hidden="true" data-active={flow.active === id ? '' : undefined} data-passed={passed ? '' : undefined} />;
}

// FlowDiagram: sơ đồ một lần chạy. Node đang xử lý được làm nổi; trên đường nối đang dùng có
// một chấm chạy (Agent ⇄ model và Agent ⇄ tool là chặng đi rồi về). Thuần trình bày: mọi
// trạng thái do người gọi tính từ dữ liệu của API.
export function FlowDiagram({ flow, labels, actions }: { flow: Flow; labels: FlowLabels; actions?: ReactNode }) {
  const used = (id: FlowNodeId) => flow.nodes[id].state !== 'idle';
  return (
    <section className="card flow-card" aria-label={labels.title}>
      <div className="row"><h2>{labels.title}</h2><span className="spacer" />{actions}</div>
      <div className="flow">
        <Node id="user" flow={flow} labels={labels} />
        <Edge id="user-agent" flow={flow} passed />
        <Node id="agent" flow={flow} labels={labels} />
        <div className="flow-branch">
          <div className="flow-lane"><Edge id="agent-model" flow={flow} passed={used('model')} /><Node id="model" flow={flow} labels={labels} /></div>
          <div className="flow-lane"><Edge id="agent-tool" flow={flow} passed={used('tool')} /><Node id="tool" flow={flow} labels={labels} /></div>
        </div>
        <Edge id="agent-result" flow={flow} passed={used('result')} />
        <Node id="result" flow={flow} labels={labels} />
      </div>
    </section>
  );
}
