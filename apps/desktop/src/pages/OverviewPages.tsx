import { Activity, AlertTriangle, Bot, CheckCircle2, Clock3, Cpu, Database, HardDrive, MemoryStick, Mic2, Network, ShieldCheck, Smartphone, Workflow } from "lucide-react";
import { Link } from "react-router-dom";
import { formatBytes, formatDate, formatDuration, sentenceCase } from "../lib/format";
import { useDesktop } from "../state/DesktopContext";
import { Badge, ConfirmButton, DefinitionList, MetricCard, PageHeader, Panel, Progress } from "../components/ui";

export function DashboardPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const activeTasks = snapshot.tasks.filter((task) => ["RUNNING", "ASSIGNED", "WAITING_FOR_CHILDREN", "WAITING_FOR_APPROVAL"].includes(task.status));
  const pendingApprovals = snapshot.approvals.filter((approval) => approval.status === "pending");
  const onlineDevices = snapshot.devices.filter((device) => device.status === "online");
  const activeVoice = snapshot.voice_sessions.filter((session) => session.state !== "ENDED");
  const currentTask = activeTasks[0];

  return (
    <>
      <PageHeader
        eyebrow="Local control plane"
        title="Good morning. Veqri is ready."
        description="Core, agents, devices, and policy decisions at a glance. All data stays on this machine unless an enabled adapter sends it elsewhere."
        actions={
          snapshot.core.emergency_stop ? (
            <ConfirmButton
              label="Clear emergency stop"
              title="Allow tool execution again?"
              description="Core will resume evaluating new tool requests through the normal policy engine. Existing denials and approvals are unchanged."
              confirmLabel="Clear emergency stop"
              danger={false}
              disabled={busyAction !== null}
              onConfirm={() => runAction({ type: "core.emergency_stop.set", enabled: false }, "clear emergency stop")}
            />
          ) : (
            <ConfirmButton
              label="Emergency stop"
              title="Stop all new tool execution?"
			  description="Core will reject new tool invocations. Running work continues unless you cancel its task separately; conversations and inspection remain available."
              confirmLabel="Activate emergency stop"
              disabled={busyAction !== null}
              onConfirm={() => runAction({ type: "core.emergency_stop.set", enabled: true }, "emergency stop")}
            />
          )
        }
      />

      <div className="metric-grid">
        <MetricCard label="Core status" value={sentenceCase(snapshot.core.status)} detail={`${snapshot.core.version} · up ${formatDuration(snapshot.core.uptime_seconds)}`} icon={<Activity size={20} />} tone={snapshot.core.status === "healthy" ? "positive" : "warning"} />
        <MetricCard label="Active tasks" value={activeTasks.length} detail={`${snapshot.core.queue.queued} queued · ${snapshot.core.queue.blocked} blocked`} icon={<Workflow size={20} />} />
        <MetricCard label="Pending approvals" value={pendingApprovals.length} detail={pendingApprovals.length ? "Review before tokens expire" : "No decisions waiting"} icon={<ShieldCheck size={20} />} tone={pendingApprovals.length ? "warning" : "positive"} />
        <MetricCard label="Connected" value={`${onlineDevices.length} device${onlineDevices.length === 1 ? "" : "s"}`} detail={`${activeVoice.length} active voice session`} icon={<Smartphone size={20} />} />
      </div>

      <div className="dashboard-grid">
        <Panel title="Current work" description="Agent and tool activity from the durable task graph" className="dashboard-grid__wide" action={<Link className="text-link" to="/tasks">All tasks</Link>}>
          {currentTask ? (
            <div className="featured-task">
              <div className="featured-task__top">
                <div><Badge value={currentTask.status} /><h3><Link to={`/tasks/${currentTask.id}`}>{currentTask.goal}</Link></h3></div>
                <span className="agent-chip"><Bot size={14} aria-hidden="true" />{currentTask.assigned_agent_name ?? "Awaiting agent"}</span>
              </div>
              <p>{currentTask.summary}</p>
              <Progress value={currentTask.progress_percent} label="Task progress" />
              <div className="task-meta-row">
                <span><Clock3 size={14} aria-hidden="true" />Started {formatDate(currentTask.started_at)}</span>
                {currentTask.current_tool ? <span><HardDrive size={14} aria-hidden="true" />{currentTask.current_tool}</span> : null}
                <span>{activeTasks.length - 1} other active</span>
              </div>
            </div>
          ) : (
            <div className="inline-empty"><CheckCircle2 size={20} aria-hidden="true" /><span>No active tasks. The durable queue is caught up.</span></div>
          )}
        </Panel>

        <Panel title="Voice link" description="Realtime Android session" action={<Link className="text-link" to="/voice">Inspect</Link>}>
          {activeVoice[0] ? (
            <div className="voice-compact">
              <div className="voice-orb" aria-hidden="true"><Mic2 size={22} /></div>
              <div><Badge value={activeVoice[0].state} /><h3>{activeVoice[0].device_name}</h3><p>“{activeVoice[0].partial_transcript}”</p></div>
              <dl><div><dt>Round trip</dt><dd>{activeVoice[0].round_trip_ms} ms</dd></div><div><dt>Packet loss</dt><dd>{activeVoice[0].packet_loss_percent}%</dd></div></dl>
            </div>
          ) : <div className="inline-empty"><Mic2 size={20} aria-hidden="true" /><span>No active voice session.</span></div>}
        </Panel>

        <Panel title="Needs your attention" description="Explicit decisions and degraded adapters" action={<Link className="text-link" to="/approvals">Review approvals</Link>}>
          <div className="attention-list">
            {pendingApprovals.slice(0, 2).map((approval) => (
              <Link to="/approvals" className="attention-item" key={approval.id}>
                <span className="attention-item__icon"><ShieldCheck size={17} /></span>
                <span><strong>{approval.permission}</strong><small>{approval.requested_by_agent} · expires {formatDate(approval.expires_at)}</small></span>
              </Link>
            ))}
            {snapshot.connectors.filter((connector) => connector.health === "degraded").map((connector) => (
              <Link to="/connectors" className="attention-item" key={connector.id}>
                <span className="attention-item__icon is-warning"><AlertTriangle size={17} /></span>
                <span><strong>{connector.name}</strong><small>{connector.error}</small></span>
              </Link>
            ))}
          </div>
        </Panel>

        <Panel title="Security posture" description="The important defaults currently enforced">
          <ul className="posture-list">
			<li><CheckCircle2 size={16} aria-hidden="true" /><span><strong>Configured bind</strong><small>{snapshot.core.bind_address}; non-loopback requires Core TLS</small></span></li>
			<li><CheckCircle2 size={16} aria-hidden="true" /><span><strong>Secret-reference support</strong><small>Production adapters accept keychain or environment references; direct values are development-only</small></span></li>
            <li><CheckCircle2 size={16} aria-hidden="true" /><span><strong>Destructive actions gated</strong><small>Single-use expiring approvals</small></span></li>
            <li><CheckCircle2 size={16} aria-hidden="true" /><span><strong>Diagnostics redacted</strong><small>Enabled for exported bundles</small></span></li>
          </ul>
        </Panel>
      </div>
    </>
  );
}

export function CoreHealthPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  if (!snapshot) return null;
  const core = snapshot.core;

  return (
    <>
      <PageHeader
        eyebrow="Runtime"
        title="Core health"
        description="Process, durable storage, queue, and local network posture for the authoritative Veqri daemon."
        actions={<Badge value={core.status} label={`Core is ${core.status}`} />}
      />
      <div className="metric-grid metric-grid--three">
        <MetricCard label="Processor" value={core.cpu_percent === null ? "Not measured" : `${core.cpu_percent.toFixed(1)}%`} detail={core.cpu_percent === null ? "Portable sampling is disabled" : "Current core process"} icon={<Cpu size={20} />} />
        <MetricCard label="Memory" value={core.memory_bytes === null ? "Not measured" : formatBytes(core.memory_bytes)} detail={core.memory_bytes === null ? "Portable RSS sampling is disabled" : "Resident set size"} icon={<MemoryStick size={20} />} />
        <MetricCard label="Durable queue" value={core.queue.running} detail={`${core.queue.queued} queued · ${core.queue.blocked} blocked`} icon={<Database size={20} />} tone={core.queue.blocked ? "warning" : "positive"} />
      </div>
      <div className="two-column">
        <Panel title="Core process" description="Authenticated localhost service details">
          <DefinitionList items={[
            { term: "Status", detail: <Badge value={core.status} /> },
            { term: "Version", detail: core.version },
            { term: "Protocol", detail: `v${core.protocol_version}` },
            { term: "Started", detail: formatDate(core.started_at) },
            { term: "Uptime", detail: formatDuration(core.uptime_seconds) },
            { term: "Mode", detail: sentenceCase(core.service_mode) },
            { term: "Bind address", detail: <code>{core.bind_address}</code> },
          ]} />
        </Panel>
        <Panel title="SQLite store" description="Core remains authoritative; clients are caches">
          <DefinitionList items={[
            { term: "Integrity", detail: <Badge value={core.database.status} /> },
            { term: "Path", detail: <code className="breakable">{core.database.path}</code> },
            { term: "Size", detail: formatBytes(core.database.size_bytes) },
            { term: "Journal", detail: core.database.wal_enabled ? "WAL enabled" : "Rollback journal" },
            { term: "Migration", detail: `Schema ${core.database.migration_version}` },
          ]} />
        </Panel>
      </div>
      <Panel title="Safety controls" description="Enforced inside Core, not in this desktop interface" className="safety-panel">
        <div className="safety-control">
          <div className={`safety-control__icon ${core.emergency_stop ? "is-danger" : ""}`}><ShieldCheck size={22} aria-hidden="true" /></div>
          <div><h3>{core.emergency_stop ? "Emergency stop active" : "Normal policy evaluation"}</h3><p>{core.emergency_stop ? "New tool execution is blocked. Inspection and conversations remain available." : "Every tool request still passes capability policy and approval checks."}</p></div>
          <ConfirmButton
            label={core.emergency_stop ? "Clear stop" : "Activate stop"}
            title={core.emergency_stop ? "Resume policy-controlled execution?" : "Block all new tool execution?"}
			description={core.emergency_stop ? "New requests will again be evaluated by normal Core policy." : "Core will reject new tool execution without stopping conversations, inspection, or already-running work."}
            confirmLabel={core.emergency_stop ? "Resume execution" : "Activate emergency stop"}
            danger={!core.emergency_stop}
            disabled={busyAction !== null}
            onConfirm={() => runAction({ type: "core.emergency_stop.set", enabled: !core.emergency_stop }, "core emergency stop")}
          />
        </div>
      </Panel>
      <Panel title="Local interfaces" description="All remote access is opt-in and must be configured separately">
        <div className="interface-grid">
		  <div><Network size={18} aria-hidden="true" /><strong>HTTP(S)</strong><code>{core.bind_address}/api/v1</code></div>
		  <div><Activity size={18} aria-hidden="true" /><strong>WebSocket</strong><code>{core.bind_address}/api/v1/events</code></div>
        </div>
      </Panel>
    </>
  );
}
