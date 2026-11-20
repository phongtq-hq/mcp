// Inspector của bản đồ: chi tiết của node hoặc cạnh đang chọn, không rời bản đồ. Chỉ hiện
// thứ API thật sự trả về; chỗ API chưa có số liệu (số lần gọi theo cạnh, độ trễ của server)
// thì không có mục đó, thay vì một con số minh hoạ.
import { useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { get, soft, type Page, type Schemas } from '@/api/client';
import { Inspector, InspectorSection } from '@/components/inspector';
import { Badge, KV, RiskBadge, StatusBadge } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { fmtMoney, fmtNumber, fmtRelative, fmtTime, shortId } from '@/lib/format';
import { json, visible } from '@/lib/visible';
import type { Access, AgentNode, ControlNode, GraphEdge, GraphModel, GraphNode, ServerNode, SystemNode, ToolNode } from './model';

interface Props {
  model: GraphModel;
  selected: string;
  // Số node đang được chọn cùng lúc (chọn nhiều): inspector hiện node đầu tiên.
  count: number;
  onSelect: (id: string) => void;
  onClose: () => void;
}

const ACCESS_TONE: Record<Access, string> = { allow: 'ok', approval: 'warn', deny: 'bad' };

function Pick({ node, onSelect, children }: { node: GraphNode | undefined; onSelect: (id: string) => void; children?: ReactNode }) {
  if (!node) return <span className="muted">—</span>;
  return <button type="button" className="link-btn" onClick={() => onSelect(node.id)}>{children ?? node.label}</button>;
}

function Rows({ empty, children }: { empty: string; children: ReactNode[] }) {
  if (children.length === 0) return <p className="muted small">{empty}</p>;
  return <ul className="inspector-list">{children}</ul>;
}

function AgentPanel({ node, model, onSelect }: { node: AgentNode; model: GraphModel; onSelect: (id: string) => void }) {
  const byId = new Map(model.nodes.map(n => [n.id, n]));
  const uses = model.edges.filter(e => e.kind === 'uses' && e.source === node.id);
  const delegates = model.edges.filter(e => e.kind === 'delegates' && e.source === node.id);
  const access = [...(model.access.get(node.id) ?? [])];
  const refs = node.config?.tools ?? [];
  const recent = useQuery({
    queryKey: ['agent-map', 'agent-executions', node.agentId],
    queryFn: () => soft(get<Page<Schemas['Execution']>>(`/projects/${node.projectId}/executions?agent_id=${node.agentId}&limit=5`)),
  });
  const cfg = node.config;
  return (
    <>
      <InspectorSection title={t('mapOverview')}>
        {node.description ? <p className="answer small">{visible(node.description)}</p> : null}
        <KV pairs={[
          [t('status'), <StatusBadge status={node.status} />],
          [t('project'), <Link to={`/projects/${node.projectId}/agents`}>{node.projectName}</Link>],
          [t('environment'), node.environment === 'prod' ? t('envProd') : t('envNonProd')],
          [t('slug'), <span className="mono">{node.slug}</span>],
          [t('version'), node.version === null ? '—' : node.published ? String(node.version) : t('mapVersionDraft', { version: node.version })],
          [t('risk'), node.risk ? <RiskBadge risk={node.risk} /> : cfg ? t('mapNoTools') : t('mapUnknownAccess')],
          cfg ? [t('mapModelPolicy'), cfg.model.policy === 'tiered' ? t('mapPolicyTiered', { tier: cfg.model.default_tier ?? 'standard' }) : t('mapPolicyFixed')] : null,
          cfg ? [t('mapLimits'), t('mapLimitsValue', { iterations: cfg.limits.max_iterations, calls: cfg.limits.max_tool_calls, seconds: cfg.limits.timeout_seconds })] : null,
          cfg?.limits.budget ? [t('mapBudget'), fmtMoney(cfg.limits.budget)] : null,
        ]} />
        {cfg ? null : <p className="muted small">{t('mapNoConfig')}</p>}
      </InspectorSection>
      <InspectorSection title={t('mapPermissions')}>
        <Rows empty={t('mapNoServers')}>
          {uses.map(e => {
            const always = refs.some(r => 'server:' + r.server_id === e.target && r.require_approval === 'always');
            return (
              <li key={e.id}>
                <Pick node={byId.get(e.target)} onSelect={onSelect} />
                <span className="spacer" />
                <span className="small muted">{t('mapAllowedDenied', { allowed: e.allowed ?? 0, denied: e.denied ?? 0 })}</span>
                {always ? <Badge tone="warn">{t('mapAlwaysApproval')}</Badge> : null}
                {e.state === 'broken' ? <Badge tone="bad">{t('edge_broken')}</Badge> : null}
              </li>
            );
          })}
        </Rows>
      </InspectorSection>
      <InspectorSection title={t('tools')}>
        <Rows empty={t('mapNoToolsHint')}>
          {access.map(([toolId, decision]) => {
            const tool = byId.get(toolId);
            if (!tool || tool.kind !== 'tool') return null;
            return (
              <li key={toolId}>
                <Pick node={tool} onSelect={onSelect}><span className="mono">{tool.label}</span></Pick>
                <span className="spacer" />
                <RiskBadge risk={tool.risk} />
                <Badge tone={tool.enabled ? ACCESS_TONE[decision] : undefined}>{tool.enabled ? t(`access_${decision}`) : t('disabledTool')}</Badge>
              </li>
            );
          })}
        </Rows>
        <p className="small muted">{t('mapDecisionNote')}</p>
      </InspectorSection>
      {cfg?.delegation ? (
        <InspectorSection title={t('mapDelegation')}>
          <Rows empty={t('mapNoDelegation')}>
            {delegates.map(e => <li key={e.id}><Pick node={byId.get(e.target)} onSelect={onSelect} /></li>)}
          </Rows>
          <p className="small muted">{t('mapMaxDepth', { depth: cfg.delegation.max_depth })}</p>
        </InspectorSection>
      ) : null}
      <InspectorSection title={t('mapRecentExecutions')}>
        {recent.isPending ? <p className="muted small">{t('loading')}</p> : recent.data ? (
          <Rows empty={t('mapNoExecutions')}>
            {recent.data.data.map(x => (
              <li key={x.id}>
                <Link className="mono" to={'/executions/' + x.id}>{shortId(x.id)}</Link>
                <StatusBadge status={x.status} />
                <span className="spacer" />
                <span className="small muted">{fmtRelative(x.created_at)}</span>
              </li>
            ))}
          </Rows>
        ) : <p className="muted small">{t('mapNoAccess')}</p>}
      </InspectorSection>
    </>
  );
}

function ServerPanel({ node, model, onSelect }: { node: ServerNode; model: GraphModel; onSelect: (id: string) => void }) {
  const byId = new Map(model.nodes.map(n => [n.id, n]));
  const agents = model.edges.filter(e => e.kind === 'uses' && e.target === node.id);
  const tools = model.nodes.filter((n): n is ToolNode => n.kind === 'tool' && n.serverNode === node.id);
  const system = node.systemId ? byId.get(node.systemId) : undefined;
  return (
    <>
      <InspectorSection title={t('mapOverview')}>
        <KV pairs={[
          [t('status'), <StatusBadge status={node.status} />],
          [t('health'), tOr('health_' + node.health, node.health)],
          [t('mapTransport'), tOr('transport_' + node.transport, node.transport)],
          [t('kind'), tOr('kind_' + node.serverKind, node.serverKind)],
          node.scope ? [t('scope'), tOr('scope_' + node.scope, node.scope)] : null,
          [t('version'), node.version === null ? '—' : String(node.version)],
          system ? [t('mapEndpoint'), <Pick node={system} onSelect={onSelect}><span className="mono">{system.label}</span></Pick>] : null,
        ]} />
        {node.pendingReview ? <p><Badge tone="warn">{t('pendingReview')}</Badge></p> : null}
        <p className="small"><Link to="/mcp">{t('mapOpenRegistry')}</Link></p>
      </InspectorSection>
      <InspectorSection title={t('mapConnectedAgents')}>
        <Rows empty={t('mapNoAgentsUse')}>
          {agents.map(e => (
            <li key={e.id}>
              <Pick node={byId.get(e.source)} onSelect={onSelect} />
              <span className="spacer" />
              <Badge tone={e.state === 'ok' ? 'ok' : e.state === 'approval' ? 'warn' : 'bad'}>{t(`edge_${e.state}`)}</Badge>
            </li>
          ))}
        </Rows>
      </InspectorSection>
      <InspectorSection title={t('mapToolsOf', { enabled: node.enabledTools, total: node.tools })}>
        <Rows empty={t('noTools')}>
          {tools.map(tool => (
            <li key={tool.id}>
              <Pick node={tool} onSelect={onSelect}><span className="mono">{tool.label}</span></Pick>
              <span className="spacer" />
              <RiskBadge risk={tool.risk} />
              {tool.enabled ? null : <Badge>{t('disabledTool')}</Badge>}
            </li>
          ))}
        </Rows>
      </InspectorSection>
    </>
  );
}

function ToolPanel({ node, model, onSelect }: { node: ToolNode; model: GraphModel; onSelect: (id: string) => void }) {
  const byId = new Map(model.nodes.map(n => [n.id, n]));
  const agents = [...model.access].flatMap(([agent, decisions]) => {
    const decision = decisions.get(node.id);
    return decision ? [{ agent, decision }] : [];
  });
  // Lịch sử gọi lấy từ audit (`mcp_tool.call`). Audit chỉ org_admin đọc được: thiếu quyền
  // thì mục này nói rõ, không để trống như thể chưa từng có lời gọi.
  const calls = useQuery({
    queryKey: ['agent-map', 'tool-calls', node.toolId],
    queryFn: () => soft(get<Page<Schemas['AuditLog']>>(`/audit-logs?action=mcp_tool.call&resource_type=mcp_tool&resource_id=${node.toolId}&limit=8`)),
  });
  return (
    <>
      <InspectorSection title={t('mapOverview')}>
        {/* Mô tả do MCP server khai (có thể là server ngoài): làm lộ ký tự ẩn. */}
        {node.description ? <p className="answer small">{visible(node.description)}</p> : null}
        <KV pairs={[
          [t('servers'), <Pick node={byId.get(node.serverNode)} onSelect={onSelect} />],
          [t('risk'), <RiskBadge risk={node.risk} />],
          [t('sideEffect'), tOr('se_' + node.sideEffect, node.sideEffect)],
          [t('status'), node.enabled ? <Badge tone="ok">{t('enabled')}</Badge> : <Badge>{t('disabledTool')}</Badge>],
        ]} />
      </InspectorSection>
      <InspectorSection title={t('mapPermissions')}>
        <Rows empty={t('mapNoAgentsUse')}>
          {agents.map(({ agent, decision }) => (
            <li key={agent}>
              <Pick node={byId.get(agent)} onSelect={onSelect} />
              <span className="spacer" />
              <Badge tone={ACCESS_TONE[decision]}>{t(`access_${decision}`)}</Badge>
            </li>
          ))}
        </Rows>
        <p className="small muted">{t('mapDecisionNote')}</p>
      </InspectorSection>
      <InspectorSection title={t('mapInputSchema')}>
        <pre>{json(node.inputSchema)}</pre>
      </InspectorSection>
      <InspectorSection title={t('mapRecentCalls')}>
        {calls.isPending ? <p className="muted small">{t('loading')}</p> : calls.data ? (
          <Rows empty={t('mapNoCalls')}>
            {calls.data.data.map(log => (
              <li key={log.id}>
                <Badge tone={log.outcome === 'success' ? 'ok' : log.outcome === 'denied' ? 'warn' : 'bad'}>{tOr('outcome_' + log.outcome, log.outcome)}</Badge>
                {log.execution_id ? <Link className="mono" to={'/executions/' + log.execution_id}>{shortId(log.execution_id)}</Link> : null}
                <span className="spacer" />
                <span className="small muted">{fmtTime(log.occurred_at)}</span>
              </li>
            ))}
          </Rows>
        ) : <p className="muted small">{t('mapAuditNoAccess')}</p>}
      </InspectorSection>
    </>
  );
}

function ControlPanel({ node }: { node: ControlNode }) {
  return (
    <InspectorSection title={t('mapOverview')}>
      <KV pairs={[
        [t('mapActiveAgents'), `${fmtNumber(node.activeAgents)} / ${fmtNumber(node.agents)}`],
        [t('mapHealthyServers'), `${fmtNumber(node.healthyServers)} / ${fmtNumber(node.servers)}`],
        [t('tools'), fmtNumber(node.tools)],
        [t('mapDisabledTools'), fmtNumber(node.disabledTools)],
        [t('pendingReview'), fmtNumber(node.pendingReviews)],
        [t('statPendingApprovals'), node.pendingApprovals === null ? '—' : <Link to="/approvals">{fmtNumber(node.pendingApprovals)}</Link>],
      ]} />
    </InspectorSection>
  );
}

function SystemPanel({ node, model, onSelect }: { node: SystemNode; model: GraphModel; onSelect: (id: string) => void }) {
  const servers = model.nodes.filter((n): n is ServerNode => n.kind === 'server' && n.systemId === node.id);
  return (
    <>
      <InspectorSection title={t('mapOverview')}>
        <KV pairs={[[t('mapEndpoint'), <span className="mono">{node.host}</span>]]} />
        <p className="small muted">{t('mapSystemHint')}</p>
      </InspectorSection>
      <InspectorSection title={t('servers')}>
        <Rows empty={t('noServers')}>
          {servers.map(s => <li key={s.id}><Pick node={s} onSelect={onSelect} /><span className="spacer" /><StatusBadge status={s.status} /></li>)}
        </Rows>
      </InspectorSection>
    </>
  );
}

function EdgePanel({ edge, model, onSelect }: { edge: GraphEdge; model: GraphModel; onSelect: (id: string) => void }) {
  const byId = new Map(model.nodes.map(n => [n.id, n]));
  return (
    <InspectorSection title={t('mapRelation')}>
      <KV pairs={[
        [t('mapSource'), <Pick node={byId.get(edge.source)} onSelect={onSelect} />],
        [t('mapTarget'), <Pick node={byId.get(edge.target)} onSelect={onSelect} />],
        [t('kind'), t(`rel_${edge.kind}`)],
        [t('mapDecision'), <Badge tone={edge.state === 'ok' ? 'ok' : edge.state === 'approval' ? 'warn' : 'bad'}>{t(`edge_${edge.state}`)}</Badge>],
        edge.kind === 'uses' ? [t('tools'), t('mapAllowedDenied', { allowed: edge.allowed ?? 0, denied: edge.denied ?? 0 })] : null,
      ]} />
      <p className="small muted">{t(`edgeHint_${edge.state}`)}</p>
      <p className="small muted">{t('mapDecisionNote')}</p>
    </InspectorSection>
  );
}

const ICONS = { control: 'layers', agent: 'bot', server: 'server', tool: 'tool', system: 'globe' } as const;

export function NodeInspector({ model, selected, count, onSelect, onClose }: Props) {
  const node = model.nodes.find(n => n.id === selected);
  // Cạnh "tới hệ thống đích" được dựng ở lớp nhìn; nó không có trong model và không có
  // chi tiết gì để hiện ngoài hai đầu của nó.
  const edge = node ? undefined : model.edges.find(e => e.id === selected);
  if (!node && !edge) return null;
  const more = count > 1 ? <p className="small muted">{t('mapMultiSelected', { count })}</p> : null;
  if (edge) {
    return <Inspector icon="map" title={t(`rel_${edge.kind}`)} subtitle={t('mapRelation')} onClose={onClose}><EdgePanel edge={edge} model={model} onSelect={onSelect} /></Inspector>;
  }
  if (!node) return null;
  return (
    <Inspector icon={ICONS[node.kind]} title={node.label} subtitle={t(`nodeKind_${node.kind}`)} onClose={onClose} key={node.id}>
      {more}
      {node.kind === 'agent' ? <AgentPanel node={node} model={model} onSelect={onSelect} /> : null}
      {node.kind === 'server' ? <ServerPanel node={node} model={model} onSelect={onSelect} /> : null}
      {node.kind === 'tool' ? <ToolPanel node={node} model={model} onSelect={onSelect} /> : null}
      {node.kind === 'control' ? <ControlPanel node={node} /> : null}
      {node.kind === 'system' ? <SystemPanel node={node} model={model} onSelect={onSelect} /> : null}
    </Inspector>
  );
}
