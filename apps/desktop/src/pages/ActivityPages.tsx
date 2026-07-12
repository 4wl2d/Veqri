import {
  AlertOctagon,
  ArrowDown,
  ArrowRight,
  ArrowUp,
  Bot,
  Check,
  Clock3,
  FileText,
  KeyRound,
  Link2,
  MessageSquareText,
  Mic2,
  Network,
  PhoneOff,
  RotateCcw,
  ShieldAlert,
  Smartphone,
  Square,
  Terminal,
  TimerReset,
  Workflow,
  X,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { Approval, Task, TaskStatus } from "../api/types";
import { Badge, Button, ConfirmButton, DefinitionList, EmptyState, PageHeader, Panel, Progress } from "../components/ui";
import { formatBytes, formatDate, formatDuration, sentenceCase } from "../lib/format";
import { useDesktop } from "../state/DesktopContext";

export function DevicesPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  return (
    <>
      <PageHeader eyebrow="Trust boundary" title="Android devices" description="Paired identities, key lifetime, presence, and negotiated device capabilities." />
      {snapshot.devices.length === 0 ? (
        <EmptyState title="No paired devices" description="Pair Android with a short-lived one-time code from Veqri Core." />
      ) : (
        <div className="card-grid">
          {snapshot.devices.map((device) => (
            <Panel key={device.id} className="device-card">
              <div className="entity-heading">
                <span className="entity-icon"><Smartphone size={21} aria-hidden="true" /></span>
                <div><h2>{device.name}</h2><p>{device.model}</p></div>
                <Badge value={device.status} />
              </div>
              <DefinitionList items={[
                { term: "Connection", detail: sentenceCase(device.network) },
                { term: "Last seen", detail: formatDate(device.last_seen_at) },
                { term: "Paired", detail: formatDate(device.paired_at) },
				{ term: "Credential version", detail: String(device.key_version) },
                { term: "App", detail: device.app_version },
              ]} />
              <div className="tag-list" aria-label="Device capabilities">{device.capabilities.map((capability) => <span key={capability}>{capability}</span>)}</div>
              <div className="panel-footer">
                <span className="muted-id">{device.id}</span>
                {device.status !== "revoked" ? (
                  <ConfirmButton
                    label="Revoke device"
                    title={`Revoke ${device.name}?`}
                    description="Its persistent identity will be rejected immediately. Pairing again will create a new identity and credential."
                    confirmLabel="Revoke device"
                    disabled={busyAction !== null}
                    onConfirm={() => runAction({ type: "device.revoke", device_id: device.id }, `revoke ${device.id}`)}
                  />
                ) : null}
              </div>
            </Panel>
          ))}
        </div>
      )}
    </>
  );
}

export function VoiceSessionsPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const sessions = snapshot.voice_sessions.filter((session) => session.state !== "ENDED");
  return (
    <>
      <PageHeader eyebrow="Realtime audio" title="Active voice sessions" description="Signaling state, transcript activity, task delegation, and WebRTC network quality." />
      {sessions.length === 0 ? (
        <EmptyState title="No active voice sessions" description="A paired Android device can start a call, or Core can initiate one through an approved notification action." />
      ) : sessions.map((session) => (
        <Panel key={session.id} className="voice-session-panel">
          <div className="voice-session-hero">
            <div className="voice-visual" aria-hidden="true"><Mic2 size={26} /><span /><span /><span /></div>
            <div><p className="eyebrow">{session.transport} · {session.codec}</p><h2>{session.device_name}</h2><div className="voice-status-row"><Badge value={session.state} /><span>{formatDuration(session.duration_seconds)}</span></div></div>
            <ConfirmButton
              label="End session"
              title="End this voice session?"
              description="Audio and call signaling will stop. Delegated background tasks will continue unless cancelled separately."
              confirmLabel="End voice session"
              disabled={busyAction !== null}
              onConfirm={() => runAction({ type: "voice.end", session_id: session.id }, `end ${session.id}`)}
            />
          </div>
          <div className="transcript-card" aria-live="polite">
            <span><span className="pulse-dot" aria-hidden="true" />Live partial transcript</span>
            <p>{session.partial_transcript || "Listening for speech…"}</p>
          </div>
          <div className="voice-metrics">
            <div><Network size={17} aria-hidden="true" /><span><strong>{session.round_trip_ms} ms</strong><small>Round trip</small></span></div>
            <div><Workflow size={17} aria-hidden="true" /><span><strong>{session.active_task_count}</strong><small>Active tasks</small></span></div>
            <div><Link2 size={17} aria-hidden="true" /><span><strong>{session.packet_loss_percent}%</strong><small>Packet loss</small></span></div>
            <div><Clock3 size={17} aria-hidden="true" /><span><strong>{formatDate(session.started_at)}</strong><small>Started</small></span></div>
          </div>
          <p className="policy-note"><ShieldAlert size={16} aria-hidden="true" />Interrupting speech stops TTS immediately; it never cancels delegated work by itself.</p>
        </Panel>
      ))}
    </>
  );
}

export function ConversationsPage() {
  const { snapshot } = useDesktop();
  const [query, setQuery] = useState("");
  if (!snapshot) return null;
  const filtered = snapshot.conversations.filter((conversation) => `${conversation.title} ${conversation.last_message}`.toLowerCase().includes(query.toLowerCase()));
  return (
    <>
      <PageHeader eyebrow="Dialog history" title="Conversations" description="Text and voice context across Android, desktop, and configured messaging connectors." actions={<label className="search-field"><span className="sr-only">Search conversations</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search conversations" /></label>} />
      {filtered.length === 0 ? <EmptyState title="No matching conversations" description="Try a different title or message search." /> : (
        <div className="conversation-list">
          {filtered.map((conversation) => (
            <article className="conversation-row" key={conversation.id}>
              <span className="entity-icon"><MessageSquareText size={19} aria-hidden="true" /></span>
              <div className="conversation-row__body">
                <div><h2>{conversation.title}</h2><span>{formatDate(conversation.updated_at)}</span></div>
                <p>{conversation.last_message}</p>
                <div className="task-meta-row"><Badge value={conversation.source} /><span>{conversation.participant}</span><span>{conversation.turn_count} turns</span><span>{conversation.active_task_count} active tasks</span><span>Retention: {sentenceCase(conversation.retention)}</span></div>
              </div>
            </article>
          ))}
        </div>
      )}
    </>
  );
}

const taskFilterOptions = ["active", "all", "completed", "failed"] as const;
type TaskFilter = (typeof taskFilterOptions)[number];

function taskMatchesFilter(task: Task, filter: TaskFilter): boolean {
  if (filter === "all") return true;
  if (filter === "completed") return task.status === "COMPLETED" || task.status === "PARTIALLY_COMPLETED";
  if (filter === "failed") return ["FAILED", "BLOCKED", "TIMED_OUT", "CANCELLED"].includes(task.status);
  return !["COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "TIMED_OUT", "CANCELLED"].includes(task.status);
}

export function TasksPage() {
  const { snapshot } = useDesktop();
  const [filter, setFilter] = useState<TaskFilter>("active");
  if (!snapshot) return null;
  const filtered = snapshot.tasks.filter((task) => taskMatchesFilter(task, filter));
  return (
    <>
      <PageHeader eyebrow="Durable orchestration" title="Tasks" description="Inspect assignment, progress, dependencies, retries, tools, artifacts, and correlation metadata." />
      <div className="segmented-control" role="group" aria-label="Filter tasks">
        {taskFilterOptions.map((option) => <button key={option} aria-pressed={filter === option} onClick={() => setFilter(option)}>{sentenceCase(option)}</button>)}
      </div>
      {filtered.length === 0 ? <EmptyState title={`No ${filter} tasks`} description="Tasks will appear here as durable Core records." /> : (
        <Panel className="table-panel">
          <div className="table-scroll">
            <table>
              <caption className="sr-only">Veqri task list</caption>
              <thead><tr><th scope="col">Task</th><th scope="col">Status</th><th scope="col">Agent / tool</th><th scope="col">Progress</th><th scope="col">Created</th><th scope="col"><span className="sr-only">Open</span></th></tr></thead>
              <tbody>{filtered.map((task) => (
                <tr key={task.id}>
                  <td><Link className="table-primary-link" to={`/tasks/${task.id}`}>{task.goal}</Link><small>{task.id}</small></td>
                  <td><Badge value={task.status} /></td>
                  <td><span>{task.assigned_agent_name ?? "Unassigned"}</span><small>{task.current_tool ?? "No tool running"}</small></td>
                  <td className="progress-cell"><Progress value={task.progress_percent} label={`${task.goal} progress`} /></td>
                  <td>{formatDate(task.created_at)}</td>
                  <td><Link className="row-arrow" to={`/tasks/${task.id}`} aria-label={`Open ${task.goal}`}><ArrowRight size={17} /></Link></td>
                </tr>
              ))}</tbody>
            </table>
          </div>
        </Panel>
      )}
    </>
  );
}

const cancellableStates: TaskStatus[] = ["QUEUED", "ASSIGNED", "RUNNING", "WAITING_FOR_CHILDREN", "WAITING_FOR_APPROVAL", "BLOCKED"];
const retryableStates: TaskStatus[] = ["FAILED", "TIMED_OUT", "CANCELLED", "PARTIALLY_COMPLETED"];
const terminalStates: TaskStatus[] = ["COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT"];

export function TaskDetailsPage() {
  const { taskId } = useParams();
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const selected = snapshot.tasks.find((task) => task.id === taskId);
  if (!selected) return <EmptyState title="Task not found" description="This task may have been removed by retention policy." action={<Link className="button button--secondary" to="/tasks">Back to tasks</Link>} />;
  const graph = snapshot.tasks.filter((task) => task.root_task_id === selected.root_task_id);
  const root = graph.find((task) => task.id === selected.root_task_id) ?? selected;
  const children = graph.filter((task) => task.parent_task_id === root.id);

  return (
    <>
      <PageHeader
        eyebrow={`Task · ${selected.id}`}
        title={selected.goal}
        description={selected.summary || "No user-facing summary has been recorded yet."}
        actions={<div className="page-actions">
          <Button icon={<ArrowDown size={15} />} disabled={busyAction !== null || selected.priority <= -100} onClick={() => void runAction({ type: "task.reprioritize", task_id: selected.id, priority: Math.max(-100, selected.priority - 10) }, `lower priority ${selected.id}`)}>Lower priority</Button>
          <Button icon={<ArrowUp size={15} />} disabled={busyAction !== null || selected.priority >= 100} onClick={() => void runAction({ type: "task.reprioritize", task_id: selected.id, priority: Math.min(100, selected.priority + 10) }, `raise priority ${selected.id}`)}>Raise priority</Button>
          {retryableStates.includes(selected.status) && selected.retry_count < selected.max_retries && !selected.allowed_tools.includes("shell") ? <Button variant="primary" icon={<RotateCcw size={15} />} disabled={busyAction !== null} onClick={() => void runAction({ type: "task.retry", task_id: selected.id }, `retry ${selected.id}`)}>Retry task</Button> : null}
          {cancellableStates.includes(selected.status) ? <ConfirmButton label="Cancel task" title="Request task cancellation?" description="Core will signal the assigned agent and cancellable tools. Completed child results remain in the graph." confirmLabel="Request cancellation" disabled={busyAction !== null} onConfirm={() => runAction({ type: "task.cancel", task_id: selected.id }, `cancel ${selected.id}`)} /> : null}
          {terminalStates.includes(selected.status) ? <ConfirmButton label="Dismiss task" title="Dismiss this task?" description="The durable record remains available to audit and correlation views, but is removed from default task lists." confirmLabel="Dismiss task" danger={false} disabled={busyAction !== null} onConfirm={() => runAction({ type: "task.dismiss", task_id: selected.id }, `dismiss ${selected.id}`)} /> : null}
        </div>}
      />
      <div className="task-detail-summary">
        <Badge value={selected.status} />
        <Progress value={selected.progress_percent} label="Selected task progress" />
        <span><Bot size={15} aria-hidden="true" />{selected.assigned_agent_name ?? "Unassigned"}</span>
        <span><Terminal size={15} aria-hidden="true" />{selected.current_tool ?? "No tool active"}</span>
      </div>
      <div className="task-detail-grid">
        <Panel title="Task graph" description={`${graph.length} durable node${graph.length === 1 ? "" : "s"}`} className="task-graph-panel">
          <TaskNode task={root} selectedId={selected.id} />
          {children.length ? <div className="graph-connector" aria-hidden="true" /> : null}
          <div className="graph-children">
            {children.map((child) => <TaskNode key={child.id} task={child} selectedId={selected.id} />)}
          </div>
        </Panel>
        <Panel title="Selected node" description="Execution and correlation metadata">
          <DefinitionList items={[
            { term: "Status", detail: <Badge value={selected.status} /> },
            { term: "Agent", detail: selected.assigned_agent_name ?? "Unassigned" },
            { term: "Current tool", detail: selected.current_tool ?? "None" },
            { term: "Created", detail: formatDate(selected.created_at) },
            { term: "Started", detail: formatDate(selected.started_at) },
            { term: "Finished", detail: formatDate(selected.finished_at) },
            { term: "Retries", detail: `${selected.retry_count} of ${selected.max_retries}` },
            { term: "Priority", detail: `${selected.priority} (higher runs first)` },
            { term: "Correlation", detail: <code className="breakable">{selected.correlation_id}</code> },
            { term: "Parent", detail: selected.parent_task_id ? <Link className="text-link" to={`/tasks/${selected.parent_task_id}`}>{selected.parent_task_id}</Link> : "Root task" },
          ]} />
          {selected.error ? <div className="error-callout"><AlertOctagon size={18} aria-hidden="true" /><div><strong>Recorded error</strong><p>{selected.error}</p></div></div> : null}
        </Panel>
      </div>
      <div className="two-column">
        <Panel title="Permissions" description="The assigned agent cannot expand these scopes">
          <div className="tag-list">{selected.allowed_tools.length ? selected.allowed_tools.map((tool) => <span key={tool}>{tool}</span>) : <span>No tools</span>}</div>
          <h3 className="subsection-title">Dependencies</h3>
          {selected.dependencies.length ? <ul className="simple-list">{selected.dependencies.map((dependency) => <li key={dependency}><Link to={`/tasks/${dependency}`}>{dependency}</Link></li>)}</ul> : <p className="muted-copy">This node has no dependency gates.</p>}
        </Panel>
        <Panel title="Artifacts" description="Files remain available even when delivery fails">
          {selected.artifacts.length ? <ul className="artifact-list">{selected.artifacts.map((artifact) => <li key={artifact.id}><FileText size={18} aria-hidden="true" /><span><strong>{artifact.name}</strong><small>{artifact.media_type} · {formatBytes(artifact.size_bytes)}</small></span></li>)}</ul> : <p className="muted-copy">No artifacts have been recorded for this task.</p>}
        </Panel>
      </div>
    </>
  );
}

function TaskNode({ task, selectedId }: { task: Task; selectedId: string }) {
  return (
    <Link to={`/tasks/${task.id}`} className={`graph-node ${task.id === selectedId ? "is-selected" : ""}`} aria-current={task.id === selectedId ? "true" : undefined}>
      <span className="graph-node__top"><Badge value={task.status} /><span>{task.progress_percent}%</span></span>
      <strong>{task.goal}</strong>
      <small>{task.assigned_agent_name ?? "Awaiting assignment"}</small>
    </Link>
  );
}

export function ApprovalsPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const pending = snapshot.approvals.filter((approval) => approval.status === "pending");
  const resolved = snapshot.approvals.filter((approval) => approval.status !== "pending");
  return (
    <>
      <PageHeader eyebrow="Human authorization" title="Pending approvals" description="Review the exact capability, arguments, requesting agent, risk, and expiration before deciding." />
      {pending.length === 0 ? <EmptyState title="No approvals waiting" description="Core will create a single-use request when policy requires your explicit decision." /> : (
        <div className="approval-list">{pending.map((approval) => (
          <ApprovalCard key={approval.id} approval={approval} busy={busyAction !== null} onResolve={(decision) => runAction({ type: "approval.resolve", approval_id: approval.id, decision }, `${decision} ${approval.id}`)} />
        ))}</div>
      )}
      {resolved.length ? <Panel title="Recently resolved" description="Final decisions remain visible in the audit log"><div className="compact-list">{resolved.map((approval) => <div key={approval.id}><span><strong>{approval.permission}</strong><small>{approval.requested_by_agent}</small></span><Badge value={approval.status} /></div>)}</div></Panel> : null}
    </>
  );
}

function ApprovalCard({ approval, busy, onResolve }: { approval: Approval; busy: boolean; onResolve: (decision: "approved" | "denied") => Promise<unknown> }) {
  return (
    <Panel className="approval-card">
      <div className="approval-card__heading">
        <span className="approval-icon"><KeyRound size={21} aria-hidden="true" /></span>
        <div><p className="eyebrow">{approval.tool_name} · requested by {approval.requested_by_agent}</p><h2>{approval.permission}</h2><p>{approval.task_goal}</p></div>
        <Badge value={approval.risk} />
      </div>
      <div className="approval-details">
        <div><h3>Why this is needed</h3><p>{approval.reason}</p></div>
        <div><h3>Exact arguments</h3><pre><code>{JSON.stringify(approval.arguments, null, 2)}</code></pre></div>
        {approval.command_preview ? <div className="command-preview"><Terminal size={17} aria-hidden="true" /><div><span>Command preview</span><code>{approval.command_preview}</code></div></div> : null}
      </div>
      <div className="approval-card__footer">
        <span><TimerReset size={15} aria-hidden="true" />Expires {formatDate(approval.expires_at)} · single use</span>
        <div><Button variant="danger" icon={<X size={15} />} disabled={busy} onClick={() => void onResolve("denied")}>Deny</Button><Button variant="primary" icon={<Check size={15} />} disabled={busy} onClick={() => void onResolve("approved")}>Approve once</Button></div>
      </div>
    </Panel>
  );
}
