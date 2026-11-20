// Node và cạnh của bản đồ, vẽ riêng cho nền tảng này (không dùng node mặc định của thư viện).
// Trạng thái luôn hiện bằng HÌNH và CHỮ, không chỉ bằng màu. Mọi tên trên node đến từ API
// (do tenant đặt hoặc do server ngoài khai): chỉ vào trang dưới dạng văn bản.
import { createContext, memo, useContext, type ReactNode } from 'react';
import { BaseEdge, getBezierPath, Handle, Position, type Edge, type EdgeProps, type EdgeTypes, type Node, type NodeProps, type NodeTypes } from '@/components/graph';
import { Icon, type IconName } from '@/components/icon';
import { StatusIcon } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtNumber } from '@/lib/format';
import type { AgentNode, ControlNode, GraphEdge, GraphNode, ServerNode, SystemNode, ToolNode } from './model';

export type MapNodeData = {
  node: GraphNode;
  // Mờ đi: không liên quan tới lựa chọn hiện tại.
  dim: boolean;
  // Thuộc lần chạy đang được theo dõi.
  live: boolean;
  // Chỉ cho server: số tool đang thu gọn và server có đang mở không.
  folded: number;
  expanded: boolean;
};
export type MapNode = Node<MapNodeData>;

export type MapEdgeData = { edge: GraphEdge; dim: boolean; live: boolean; denied: boolean };
export type MapEdge = Edge<MapEdgeData>;

// Hành động của node (mở / thu tool của một server) đi qua context, không qua data của node:
// data đổi thì node vẽ lại, hàm thì không cần.
export const MapActions = createContext<{ toggle: (serverNode: string) => void }>({ toggle: () => { /* ngoài bản đồ: không làm gì */ } });

const ICON: Record<GraphNode['kind'], IconName> = { control: 'layers', agent: 'bot', server: 'server', tool: 'tool', system: 'globe' };

function Frame({ data, selected, children }: { data: MapNodeData; selected: boolean; children: ReactNode }) {
  const { node } = data;
  return (
    <div className="map-node" data-kind={node.kind} data-tone={node.tone} data-selected={selected || undefined}
      data-dim={data.dim || undefined} data-live={data.live || undefined}>
      {node.kind !== 'control' ? <Handle type="target" position={Position.Left} isConnectable={false} /> : null}
      {children}
      {node.kind !== 'system' ? <Handle type="source" position={Position.Right} isConnectable={false} /> : null}
    </div>
  );
}

function Title({ node, sub }: { node: GraphNode; sub?: ReactNode }) {
  return (
    <div className="map-node-head">
      <span className="map-node-icon"><Icon name={ICON[node.kind]} size={15} /></span>
      <div className="map-node-title">
        <div className="map-node-name">{node.label}</div>
        {sub ? <div className="map-node-sub">{sub}</div> : null}
      </div>
    </div>
  );
}

function State({ tone, children }: { tone: string; children: ReactNode }) {
  return <span className="map-state" data-tone={tone}><StatusIcon tone={tone === 'idle' ? undefined : tone} />{children}</span>;
}

const RISK_TONE: Record<string, string> = { read: 'ok', write: 'warn', destructive: 'bad', financial: 'bad' };

export function RiskTag({ risk }: { risk: string | null }) {
  if (!risk) return <span className="map-tag">{t('mapNoTools')}</span>;
  return <span className="map-tag" data-tone={RISK_TONE[risk] ?? 'idle'}>{tOr('risk_' + risk, risk)}</span>;
}

const Control = memo(function Control({ data, selected }: NodeProps<MapNode>) {
  const node = data.node as ControlNode;
  return (
    <Frame data={data} selected={!!selected}>
      <Title node={node} sub={<State tone={node.tone}>{t(node.tone === 'ok' ? 'mapHealthOk' : node.tone === 'warn' ? 'mapHealthDegraded' : 'mapHealthBad')}</State>} />
      <dl className="map-stats">
        <div><dt>{t('mapActiveAgents')}</dt><dd>{fmtNumber(node.activeAgents)}<span className="of">/{fmtNumber(node.agents)}</span></dd></div>
        <div><dt>{t('mapHealthyServers')}</dt><dd>{fmtNumber(node.healthyServers)}<span className="of">/{fmtNumber(node.servers)}</span></dd></div>
        <div><dt>{t('statPendingApprovals')}</dt><dd>{node.pendingApprovals === null ? '—' : fmtNumber(node.pendingApprovals)}</dd></div>
        <div><dt>{t('mapDisabledTools')}</dt><dd>{fmtNumber(node.disabledTools)}</dd></div>
      </dl>
    </Frame>
  );
});

const AgentCard = memo(function AgentCard({ data, selected }: NodeProps<MapNode>) {
  const node = data.node as AgentNode;
  return (
    <Frame data={data} selected={!!selected}>
      {/* Agent giao việc cho Agent khác: cạnh đi từ đáy node này tới đỉnh node kia. */}
      <Handle id="delegate-in" type="target" position={Position.Top} isConnectable={false} />
      <Title node={node} sub={<>{node.projectName} · {node.environment === 'prod' ? t('envProd') : t('envNonProd')}</>} />
      <div className="map-node-meta">
        <State tone={node.tone}>{tOr('st_' + node.status, node.status)}</State>
        {/* Không đọc được cấu hình: "không rõ", không phải "không có tool". */}
        {node.config ? <RiskTag risk={node.risk} /> : <span className="map-tag" data-tone="warn">{t('mapUnknownAccess')}</span>}
        <span className="map-count">{node.config ? t('mapServerCount', { count: node.servers }) : '—'}</span>
      </div>
      <Handle id="delegate-out" type="source" position={Position.Bottom} isConnectable={false} />
    </Frame>
  );
});

const ServerCard = memo(function ServerCard({ data, selected }: NodeProps<MapNode>) {
  const node = data.node as ServerNode;
  const { toggle } = useContext(MapActions);
  return (
    <Frame data={data} selected={!!selected}>
      <Title node={node} sub={<>{tOr('transport_' + node.transport, node.transport)} · {tOr('kind_' + node.serverKind, node.serverKind)}</>} />
      <div className="map-node-meta">
        <State tone={node.tone}>{node.status === 'active' ? tOr('health_' + node.health, node.health) : tOr('st_' + node.status, node.status)}</State>
        {node.pendingReview ? <span className="map-tag" data-tone="warn">{t('pendingReview')}</span> : null}
        {node.tools > 0 ? (
          // nodrag / nopan: bấm nút không kéo node hay canvas.
          <button type="button" className="map-fold nodrag nopan" aria-expanded={data.expanded}
            onClick={e => { e.stopPropagation(); toggle(node.id); }}>
            <Icon name={data.expanded ? 'down' : 'right'} size={12} />{t('mapToolCount', { count: node.tools })}
          </button>
        ) : <span className="map-count">{t('mapToolCount', { count: 0 })}</span>}
      </div>
    </Frame>
  );
});

const ToolCard = memo(function ToolCard({ data, selected }: NodeProps<MapNode>) {
  const node = data.node as ToolNode;
  return (
    <Frame data={data} selected={!!selected}>
      <div className="map-node-head">
        <span className="map-node-icon"><Icon name={node.enabled ? 'tool' : 'lock'} size={14} /></span>
        <div className="map-node-title">
          <div className="map-node-name mono">{node.label}</div>
          <div className="map-node-meta">
            <RiskTag risk={node.risk} />
            {node.enabled ? null : <span className="map-tag">{t('disabledTool')}</span>}
          </div>
        </div>
      </div>
    </Frame>
  );
});

const SystemCard = memo(function SystemCard({ data, selected }: NodeProps<MapNode>) {
  const node = data.node as SystemNode;
  return (
    <Frame data={data} selected={!!selected}>
      <Title node={node} sub={t('mapSystemServers', { count: node.servers })} />
    </Frame>
  );
});

export const nodeTypes: NodeTypes = { control: Control, agent: AgentCard, server: ServerCard, tool: ToolCard, system: SystemCard };

// Cạnh quan hệ. Kiểu nét nói trạng thái (liền: dùng được; gạch: cần duyệt; chấm: bị từ chối
// hoặc đầu kia không dùng được), màu chỉ nhắc lại. Chấm chạy chỉ có trên cạnh của lần chạy
// đang được theo dõi: bản đồ không chuyển động khi không có gì đang diễn ra.
const Relation = memo(function Relation({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, data, selected }: EdgeProps<MapEdge>) {
  const [path] = getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  const state = data?.denied ? 'denied' : data?.edge.state ?? 'ok';
  const classes = ['map-edge', 'state-' + state, 'kind-' + (data?.edge.kind ?? 'uses')];
  if (selected) classes.push('selected');
  if (data?.dim) classes.push('dim');
  if (data?.live) classes.push('live');
  return (
    <>
      <BaseEdge id={id} path={path} className={classes.join(' ')} interactionWidth={18} />
      {/* offset-path là giá trị tính lúc chạy (đường cong của cạnh), đặt qua CSSOM. */}
      {data?.live ? <circle className="map-edge-dot" r="3.5" style={{ offsetPath: `path("${path}")` }} /> : null}
    </>
  );
});

export const edgeTypes: EdgeTypes = { relation: Relation };
