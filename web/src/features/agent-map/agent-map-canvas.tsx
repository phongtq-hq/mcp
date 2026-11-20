// Canvas của bản đồ: nhận lớp nhìn (node, cạnh, toạ độ, tập liên quan) và vẽ nó bằng thư viện
// đồ thị. Không đọc API và không biết dữ liệu đến từ đâu.
import { useCallback, useEffect, useMemo, useRef, type ReactNode } from 'react';
import { Background, BackgroundVariant, MiniMap, ReactFlow, useNodesState, useReactFlow } from '@/components/graph';
import { t } from '@/i18n';
import type { Point } from '@/lib/graph-layout';
import { durationMs, reducedMotion } from '@/lib/motion';
import { useTheme } from '@/lib/theme';
import { edgeTypes, nodeTypes, type MapEdge, type MapNode } from './nodes';
import type { Focus, View } from './view';

export type Mode = 'select' | 'pan';

interface Props {
  view: View;
  positions: Map<string, Point>;
  // Tập liên quan của lựa chọn (phần còn lại mờ đi) và của lần chạy đang theo dõi.
  focus: Focus | null;
  live: Focus | null;
  selected: string | null;
  expanded: ReadonlySet<string>;
  mode: Mode;
  // Đổi giá trị này = bỏ mọi vị trí người dùng đã kéo, về bố cục tự động.
  layoutNonce: number;
  onSelect: (id: string | null, count: number) => void;
  children?: ReactNode;
}

// Trên ngần này node thì chỉ vẽ phần đang nằm trong khung nhìn.
const VIRTUALIZE_FROM = 120;
const NODE_KINDS = new Set(['control', 'agent', 'server', 'tool', 'system']);
const isNodeId = (id: string) => id === 'control' || NODE_KINDS.has(id.slice(0, id.indexOf(':')));

export function fitDuration(): number {
  return reducedMotion() ? 0 : durationMs('slow');
}

export function AgentMapCanvas({ view, positions, focus, live, selected, expanded, mode, layoutNonce, onSelect, children }: Props) {
  const flow = useReactFlow<MapNode, MapEdge>();
  const theme = useTheme();
  const [nodes, setNodes, onNodesChange] = useNodesState<MapNode>([]);
  // Vị trí người dùng đã kéo: giữ qua các lần lọc / mở server, bỏ khi đặt lại bố cục.
  const moved = useRef(new Map<string, Point>());
  const nonce = useRef(layoutNonce);

  useEffect(() => {
    if (nonce.current !== layoutNonce) { nonce.current = layoutNonce; moved.current.clear(); }
    setNodes(current => {
      const before = new Map(current.map(n => [n.id, n]));
      // Giữ object node cũ (kích thước đã đo, trạng thái chọn): dựng node mới hoàn toàn buộc thư
      // viện đo lại cả đồ thị ở mỗi lần đổi lựa chọn.
      return view.nodes.map((node): MapNode => ({
        ...before.get(node.id),
        id: node.id,
        type: node.kind,
        position: moved.current.get(node.id) ?? positions.get(node.id) ?? { x: 0, y: 0 },
        selected: before.get(node.id)?.selected ?? false,
        data: {
          node,
          dim: !!focus && !focus.nodes.has(node.id) && !live?.nodes.has(node.id),
          live: !!live?.nodes.has(node.id),
          folded: view.folded.get(node.id) ?? 0,
          expanded: expanded.has(node.id),
        },
      }));
    });
  }, [view, positions, focus, live, expanded, layoutNonce, setNodes]);

  const edges = useMemo(() => view.edges.map((edge): MapEdge => ({
    id: edge.id,
    source: edge.source,
    target: edge.target,
    type: 'relation',
    sourceHandle: edge.kind === 'delegates' ? 'delegate-out' : undefined,
    targetHandle: edge.kind === 'delegates' ? 'delegate-in' : undefined,
    selected: edge.id === selected,
    // Cạnh tới hệ thống đích không có chi tiết riêng: không chọn được.
    selectable: edge.kind !== 'connects',
    data: {
      edge,
      dim: !!focus && !focus.edges.has(edge.id) && !live?.edges.has(edge.id),
      live: !!live?.edges.has(edge.id),
      denied: !!focus?.denied.has(edge.id),
    },
  })), [view.edges, focus, live, selected]);

  // Tập node đổi (lọc, mở server, đặt lại bố cục): đưa cả bản đồ về trong khung nhìn.
  const signature = useMemo(() => view.nodes.map(n => n.id).join('|'), [view.nodes]);
  useEffect(() => {
    const handle = requestAnimationFrame(() => { void flow.fitView({ duration: fitDuration(), padding: 0.15 }); });
    return () => cancelAnimationFrame(handle);
  }, [signature, layoutNonce, flow]);

  const onSelectionChange = useCallback(({ nodes: picked }: { nodes: MapNode[] }) => {
    if (picked.length > 0) onSelect(picked[0]!.id, picked.length);
    // Không còn node nào được chọn: giữ lựa chọn nếu nó là một cạnh.
    else if (selected && isNodeId(selected)) onSelect(null, 0);
  }, [onSelect, selected]);

  return (
    <ReactFlow<MapNode, MapEdge>
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      edgeTypes={edgeTypes}
      onNodesChange={onNodesChange}
      onNodeDragStop={(_, node, dragged) => { for (const n of [node, ...dragged]) moved.current.set(n.id, n.position); }}
      onSelectionDragStop={(_, dragged) => { for (const n of dragged) moved.current.set(n.id, n.position); }}
      onSelectionChange={onSelectionChange}
      onEdgeClick={(_, edge) => { if (edge.selectable !== false) onSelect(edge.id, 1); }}
      onPaneClick={() => onSelect(null, 0)}
      colorMode={theme}
      minZoom={0.15}
      maxZoom={1.8}
      nodesConnectable={false}
      deleteKeyCode={null}
      panOnDrag={mode === 'pan' ? true : [1, 2]}
      selectionOnDrag={mode === 'select'}
      onlyRenderVisibleElements={view.nodes.length > VIRTUALIZE_FROM}
      aria-label={t('navAgentMap')}
    >
      <Background variant={BackgroundVariant.Dots} gap={22} size={1.2} />
      <MiniMap position="bottom-left" pannable zoomable ariaLabel={t('mapMinimap')} nodeClassName={node => 'mm-' + (node.type ?? 'agent')} />
      {children}
    </ReactFlow>
  );
}
