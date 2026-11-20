// Chỗ DUY NHẤT import thư viện đồ thị (@xyflow/react, xem /add-ui-dependency và
// .docs/frontend-audit.md §5). Feature dùng các tên xuất ra ở đây, không import thẳng gói:
// thay thư viện sau này chạm một file. Chỉ nạp base.css của gói (bố cục tối thiểu); màu và
// hình của node / cạnh là của dự án, ở styles/app.css.
import '@xyflow/react/dist/base.css';

export {
  Background, BackgroundVariant, BaseEdge, EdgeLabelRenderer, getBezierPath, Handle, MiniMap, Panel, Position, ReactFlow, ReactFlowProvider,
  useNodesState, useReactFlow,
} from '@xyflow/react';
export type { Edge, EdgeProps, EdgeTypes, Node, NodeProps, NodeTypes, ReactFlowInstance } from '@xyflow/react';
