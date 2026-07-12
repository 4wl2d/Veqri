import { Activity, Bot, Cable, CheckCircle2, Cloud, Cpu, Gauge, Globe2, KeyRound, Mic2, PauseCircle, PlayCircle, Radio, RotateCw, Server, ShieldCheck, Sparkles, TerminalSquare, Volume2, WifiOff, Zap } from "lucide-react";
import { Badge, Button, ConfirmButton, DefinitionList, EmptyState, MetricCard, PageHeader, Panel } from "../components/ui";
import { formatDate, sentenceCase } from "../lib/format";
import { useDesktop } from "../state/DesktopContext";

export function AgentsPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const healthy = snapshot.agents.filter((agent) => agent.health === "healthy" && !agent.kill_switch).length;
  const active = snapshot.agents.reduce((total, agent) => total + agent.active_tasks, 0);
  return (
    <>
      <PageHeader eyebrow="Capability registry" title="Agents" description="Declared capabilities, minimum tool scopes, execution boundary, health, and concurrency." />
      <div className="metric-grid metric-grid--three">
        <MetricCard label="Available" value={`${healthy} / ${snapshot.agents.length}`} detail="Healthy and accepting work" icon={<Activity size={20} />} tone={healthy === snapshot.agents.length ? "positive" : "warning"} />
        <MetricCard label="Active assignments" value={active} detail="Across durable task nodes" icon={<Zap size={20} />} />
        <MetricCard label="Local execution" value={snapshot.agents.filter((agent) => ["built_in", "local_process", "local_model"].includes(agent.execution_mode)).length} detail="No remote context transfer" icon={<Cpu size={20} />} />
      </div>
      <div className="card-grid">{snapshot.agents.map((agent) => (
        <Panel className={`agent-card ${agent.kill_switch ? "is-disabled" : ""}`} key={agent.id}>
          <div className="entity-heading">
            <span className="entity-icon"><Bot size={21} aria-hidden="true" /></span>
            <div><h2>{agent.name}</h2><p>{sentenceCase(agent.execution_mode)}</p></div>
            <Badge value={agent.kill_switch ? "disabled" : agent.health} />
          </div>
          <p className="entity-description">{agent.description}</p>
          <div className="tag-list" aria-label={`${agent.name} capabilities`}>{agent.capabilities.map((capability) => <span key={capability}>{capability}</span>)}</div>
          <DefinitionList items={[
            { term: "Trust", detail: sentenceCase(agent.trust_level) },
            { term: "Load", detail: `${agent.active_tasks} of ${agent.concurrency_limit}` },
            { term: "Latency", detail: `${agent.latency_ms} ms` },
            { term: "Streaming", detail: agent.supports_streaming ? "Supported" : "No" },
            { term: "Cancellation", detail: agent.supports_cancellation ? "Supported" : "No" },
          ]} />
          <div className="scope-block"><span>Tool scopes</span><code>{agent.tool_scopes.length ? agent.tool_scopes.join(" · ") : "none"}</code></div>
          <div className="panel-footer">
            <span className="muted-id">{agent.id}</span>
            <ConfirmButton
              label={agent.kill_switch ? "Enable agent" : "Stop new work"}
              title={agent.kill_switch ? `Enable ${agent.name}?` : `Stop new work for ${agent.name}?`}
              description={agent.kill_switch ? "The agent can receive assignments within its declared capabilities again." : "Core will not assign new tasks. Existing tasks continue unless cancelled from the task graph."}
              confirmLabel={agent.kill_switch ? "Enable agent" : "Stop new work"}
              danger={!agent.kill_switch}
              disabled={busyAction !== null}
              onConfirm={() => runAction({ type: "agent.kill_switch.set", agent_id: agent.id, enabled: !agent.kill_switch }, `agent ${agent.id}`)}
            />
          </div>
        </Panel>
      ))}</div>
    </>
  );
}

export function ToolsPoliciesPage() {
  const { snapshot } = useDesktop();
  if (!snapshot) return null;
  return (
    <>
      <PageHeader eyebrow="Authorization" title="Tools & permission policies" description="Core owns these decisions. The desktop UI only displays effective scopes and policy outcomes." />
      <Panel title="Tool registry" description="Typed boundaries and effective default permissions" className="table-panel">
        <div className="table-scroll"><table>
          <caption className="sr-only">Registered tools and effective permission levels</caption>
          <thead><tr><th scope="col">Tool</th><th scope="col">Risk</th><th scope="col">Effective policy</th><th scope="col">Boundary</th><th scope="col">Running</th></tr></thead>
          <tbody>{snapshot.tools.map((tool) => <tr key={tool.id}>
            <td><span className="table-icon-title"><TerminalSquare size={17} aria-hidden="true" /><span><strong>{tool.name}</strong><small>{tool.description}</small></span></span></td>
            <td><Badge value={tool.risk} /></td>
            <td><Badge value={tool.status} /></td>
            <td><code>{tool.workspace_boundary ?? "Not applicable"}</code></td>
            <td>{tool.running_invocations}</td>
          </tr>)}</tbody>
        </table></div>
      </Panel>
      <div className="policy-list">
        <div className="section-heading"><div><h2>Policy evaluation order</h2><p>Higher-priority matching rules win; agents cannot grant themselves scopes.</p></div><ShieldCheck size={21} aria-hidden="true" /></div>
        {snapshot.policies.sort((a, b) => b.priority - a.priority).map((policy, index) => (
          <article className={`policy-row ${!policy.enabled ? "is-disabled" : ""}`} key={policy.id}>
            <span className="policy-row__priority">{String(index + 1).padStart(2, "0")}</span>
            <div><h3>{policy.name}</h3><p>{policy.description}</p><code>{policy.match_summary}</code></div>
            <div><Badge value={policy.decision} /><small>Priority {policy.priority}</small></div>
          </article>
        ))}
      </div>
      <Panel title="Non-negotiable boundaries" description="Secure local-first defaults enforced by Core">
        <div className="boundary-grid">
          <div><CheckCircle2 size={17} aria-hidden="true" /><span><strong>Privilege escalation denied</strong><small>No policy may silently authorize elevated execution.</small></span></div>
          <div><CheckCircle2 size={17} aria-hidden="true" /><span><strong>External content untrusted</strong><small>Connector messages never authorize their own side effects.</small></span></div>
          <div><CheckCircle2 size={17} aria-hidden="true" /><span><strong>Secrets are references</strong><small>Tools receive the minimum keychain-backed credential.</small></span></div>
          <div><CheckCircle2 size={17} aria-hidden="true" /><span><strong>Output is sanitized</strong><small>Secrets and injected instructions are filtered before model context.</small></span></div>
        </div>
      </Panel>
    </>
  );
}

const connectorIcons = { slack: Radio, mattermost: MessageIcon, teams: Cloud, webhook: Globe2, local_events: Server } as const;

function MessageIcon({ size = 18 }: { size?: number }) {
  return <Cable size={size} />;
}

export function ConnectorsPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  return (
    <>
      <PageHeader eyebrow="Event ingress & delivery" title="Messaging connectors" description="Official adapters normalize untrusted messages, retain reply targets, and pass every task through Core policy." />
      {snapshot.connectors.length === 0 ? <EmptyState title="No connectors configured" description="Use a deterministic simulator or configure a secret reference for a supported platform." /> : (
        <div className="connector-list">{snapshot.connectors.map((connector) => {
          const Icon = connectorIcons[connector.kind];
          return <Panel key={connector.id} className={`connector-row ${connector.kill_switch ? "is-disabled" : ""}`}>
            <div className="connector-row__main">
              <span className="entity-icon"><Icon size={20} aria-hidden="true" /></span>
              <div><div className="connector-title"><h2>{connector.name}</h2><Badge value={connector.kill_switch ? "disabled" : connector.health} /><span className="mode-label">{connector.mode}</span></div><p>{connector.target_summary}</p>{connector.error ? <small className="error-text">{connector.error}</small> : null}</div>
            </div>
            <div className="connector-stats"><span><strong>{connector.events_today}</strong><small>Events today</small></span><span><strong>{formatDate(connector.last_event_at)}</strong><small>Last event</small></span></div>
            <div className="connector-actions">
              {connector.health === "degraded" ? <Button icon={<RotateCw size={15} />} disabled={busyAction !== null} onClick={() => void runAction({ type: "connector.retry", connector_id: connector.id }, `retry ${connector.id}`)}>Retry</Button> : null}
              <ConfirmButton
                label={connector.kill_switch ? "Enable" : "Kill switch"}
                title={connector.kill_switch ? `Enable ${connector.name}?` : `Stop ${connector.name}?`}
                description={connector.kill_switch ? "New verified events can create policy-evaluated tasks again." : "Ingress and delivery stop immediately. Existing tasks remain durable and inspectable."}
                confirmLabel={connector.kill_switch ? "Enable connector" : "Stop connector"}
                danger={!connector.kill_switch}
                disabled={busyAction !== null}
                onConfirm={() => runAction({ type: "connector.kill_switch.set", connector_id: connector.id, enabled: !connector.kill_switch }, `connector ${connector.id}`)}
              />
            </div>
          </Panel>;
        })}</div>
      )}
      <p className="policy-note"><ShieldCheck size={16} aria-hidden="true" />Slack, Mattermost, and Teams integrations use official supported APIs. Simulator mode never requires credentials.</p>
    </>
  );
}

const providerIcon = { ai: Sparkles, stt: Mic2, tts: Volume2, media: Radio, push: Cloud } as const;

export function ProvidersPage() {
  const { snapshot } = useDesktop();
  if (!snapshot) return null;
  const categories = [
    { id: "ai", title: "AI providers", description: "Planning, dialog, agents, and result synthesis" },
    { id: "stt", title: "Speech to text", description: "Streaming partial and final transcripts" },
    { id: "tts", title: "Text to speech", description: "Chunked speech and immediate barge-in" },
    { id: "media", title: "Media transport", description: "WebRTC signaling and audio" },
    { id: "push", title: "Device push", description: "Wake or notify Android when not connected" },
  ] as const;
  return (
    <>
      <PageHeader eyebrow="Pluggable adapters" title="Voice & AI providers" description="Local, remote, and deterministic providers remain optional behind stable Core interfaces." />
      {categories.map((category) => {
        const providers = snapshot.providers.filter((provider) => provider.category === category.id);
        if (!providers.length) return null;
        return <section className="provider-section" key={category.id}>
          <div className="section-heading"><div><h2>{category.title}</h2><p>{category.description}</p></div></div>
          <div className="provider-grid">{providers.map((provider) => {
            const Icon = providerIcon[provider.category];
            return <article className="provider-card" key={provider.id}>
              <div className="provider-card__top"><span className="entity-icon"><Icon size={19} aria-hidden="true" /></span><Badge value={provider.enabled ? provider.health : "disabled"} /></div>
              <h3>{provider.name}</h3><p>{provider.detail}</p>
              <DefinitionList items={[
                { term: "Adapter", detail: provider.adapter },
                { term: "Mode", detail: sentenceCase(provider.mode) },
                { term: "Latency", detail: provider.latency_ms === null ? "Not measured" : `${provider.latency_ms} ms` },
                { term: "Credential", detail: provider.secret_reference ? <code>{provider.secret_reference}</code> : "Not required" },
              ]} />
            </article>;
          })}</div>
        </section>;
      })}
      <Panel title="Provider fallback policy" description="Local-first behavior when an adapter is unavailable">
        <div className="fallback-flow" aria-label="Provider fallback order">
          <span><Cpu size={17} />Local provider</span><i>then</i><span><Server size={17} />Configured remote</span><i>then</i><span><Gauge size={17} />Deterministic simulator</span><i>or</i><span><WifiOff size={17} />Explicit failure</span>
        </div>
      </Panel>
    </>
  );
}
