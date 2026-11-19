// Bảng route. Đây là nơi duy nhất biết mọi feature; feature không import lẫn nhau.
import { lazy, Suspense } from 'react';
import { Navigate, Route, Routes } from 'react-router';
import { Spinner } from '@/components/ui';
import { Shell } from './shell';

const Overview = lazy(() => import('@/features/overview/overview-page'));
const Projects = lazy(() => import('@/features/projects/projects-page'));
const ProjectLayout = lazy(() => import('@/features/projects/project-layout'));
const AgentsTab = lazy(() => import('@/features/agents/agents-tab'));
const ExecutionsTab = lazy(() => import('@/features/executions/executions-tab'));
const ExecutionPage = lazy(() => import('@/features/executions/execution-page'));
const WorkflowsTab = lazy(() => import('@/features/workflows/workflows-tab'));
const Approvals = lazy(() => import('@/features/approvals/approvals-page'));
const ApprovalDetail = lazy(() => import('@/features/approvals/approval-detail-page'));
const Cost = lazy(() => import('@/features/cost/cost-page'));
const Mcp = lazy(() => import('@/features/mcp/mcp-page'));
const Audit = lazy(() => import('@/features/audit/audit-page'));

export function AppRoutes() {
  return (
    <Suspense fallback={<div className="center"><Spinner /></div>}>
      <Routes>
        <Route element={<Shell />}>
          <Route index element={<Overview />} />
          <Route path="projects" element={<Projects />} />
          <Route path="projects/:projectId" element={<ProjectLayout />}>
            <Route index element={<ExecutionsTab />} />
            <Route path="executions" element={<ExecutionsTab />} />
            <Route path="agents" element={<AgentsTab />} />
            <Route path="workflows" element={<WorkflowsTab />} />
          </Route>
          <Route path="executions/:executionId" element={<ExecutionPage />} />
          <Route path="approvals" element={<Approvals />} />
          <Route path="approvals/:approvalId" element={<ApprovalDetail />} />
          <Route path="cost" element={<Cost />} />
          <Route path="mcp" element={<Mcp />} />
          <Route path="audit" element={<Audit />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </Suspense>
  );
}
