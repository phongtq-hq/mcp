// Bảng route. Đây là nơi duy nhất biết mọi feature; feature không import lẫn nhau.
import { lazy, Suspense, type ComponentType } from 'react';
import { Navigate, Route, Routes } from 'react-router';
import { Spinner } from '@/components/ui';
import { shouldReload } from '@/lib/chunk-reload';
import { Shell } from './shell';

// page: màn hình nạp khi cần. Nạp thất bại thường là do tab đang giữ bản build cũ (xem
// lib/chunk-reload.ts): tải lại trang một lần; nếu vừa tải lại rồi thì để lỗi nổi lên.
function page(load: () => Promise<{ default: ComponentType }>) {
  return lazy(() => load().catch((err: unknown) => {
    if (!shouldReload(Date.now())) throw err;
    location.reload();
    return new Promise<{ default: ComponentType }>(() => { /* trang đang tải lại */ });
  }));
}

const Overview = page(() => import('@/features/overview/overview-page'));
const Projects = page(() => import('@/features/projects/projects-page'));
const ProjectNew = page(() => import('@/features/projects/project-new-page'));
const ProjectLayout = page(() => import('@/features/projects/project-layout'));
const ProjectSettingsTab = page(() => import('@/features/projects/project-settings-tab'));
const Organization = page(() => import('@/features/organization/organization-page'));
const Agents = page(() => import('@/features/agents/agents-page'));
const AgentMap = page(() => import('@/features/agent-map/agent-map-page'));
const AgentsTab = page(() => import('@/features/agents/agents-tab'));
const ExecutionsTab = page(() => import('@/features/executions/executions-tab'));
const ExecutionPage = page(() => import('@/features/executions/execution-page'));
const WorkflowsTab = page(() => import('@/features/workflows/workflows-tab'));
const Approvals = page(() => import('@/features/approvals/approvals-page'));
const ApprovalDetail = page(() => import('@/features/approvals/approval-detail-page'));
const Cost = page(() => import('@/features/cost/cost-page'));
const Mcp = page(() => import('@/features/mcp/mcp-page'));
const Audit = page(() => import('@/features/audit/audit-page'));
const Models = page(() => import('@/features/models/models-page'));
const Model = page(() => import('@/features/models/model-page'));
const Providers = page(() => import('@/features/models/providers-page'));
const Provider = page(() => import('@/features/models/provider-page'));
const Routing = page(() => import('@/features/models/routing-page'));

export function AppRoutes() {
  return (
    <Suspense fallback={<div className="center"><Spinner /></div>}>
      <Routes>
        <Route element={<Shell />}>
          <Route index element={<Overview />} />
          <Route path="projects" element={<Projects />} />
          <Route path="projects/new" element={<ProjectNew />} />
          <Route path="projects/:projectId" element={<ProjectLayout />}>
            <Route index element={<ExecutionsTab />} />
            <Route path="executions" element={<ExecutionsTab />} />
            <Route path="agents" element={<AgentsTab />} />
            <Route path="workflows" element={<WorkflowsTab />} />
            <Route path="settings" element={<ProjectSettingsTab />} />
          </Route>
          <Route path="agents" element={<Agents />} />
          <Route path="agents/map" element={<AgentMap />} />
          <Route path="executions/:executionId" element={<ExecutionPage />} />
          <Route path="approvals" element={<Approvals />} />
          <Route path="approvals/:approvalId" element={<ApprovalDetail />} />
          <Route path="cost" element={<Cost />} />
          <Route path="models" element={<Models />} />
          <Route path="models/providers" element={<Providers />} />
          <Route path="models/providers/:providerId" element={<Provider />} />
          <Route path="models/routing" element={<Routing />} />
          <Route path="models/:modelId" element={<Model />} />
          <Route path="mcp" element={<Mcp />} />
          <Route path="audit" element={<Audit />} />
          <Route path="admin/organization" element={<Organization />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </Suspense>
  );
}
