import { Archive, CheckCircle2, Database, Download, FileClock, FileDown, Filter, HardDrive, KeyRound, LockKeyhole, Network, RefreshCw, Search, Server, ShieldCheck, Stethoscope, TriangleAlert } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import type { AuditEntry, DesktopSettings } from "../api/types";
import { Badge, Button, ConfirmButton, DefinitionList, EmptyState, MetricCard, PageHeader, Panel, Toggle } from "../components/ui";
import { formatBytes, formatDate, formatTime, sentenceCase } from "../lib/format";
import { useDesktop } from "../state/DesktopContext";

export function AuditPage() {
  const { snapshot } = useDesktop();
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState<AuditEntry["category"] | "all">("all");
  if (!snapshot) return null;
  const entries = snapshot.audit_entries.filter((entry) => {
    const categoryMatches = category === "all" || entry.category === category;
    const text = `${entry.action} ${entry.actor} ${entry.target} ${entry.summary} ${entry.correlation_id}`.toLowerCase();
    return categoryMatches && text.includes(query.toLowerCase());
  });
  const categories = Array.from(new Set(snapshot.audit_entries.map((entry) => entry.category)));
  return (
    <>
      <PageHeader eyebrow="Accountability" title="Audit log" description="Core records correlated agent, tool, approval, connector, reply, and security facts until the configured retention cutoff." />
      <div className="filter-bar">
        <label className="search-field search-field--wide"><Search size={16} aria-hidden="true" /><span className="sr-only">Search audit log</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search actor, action, target, correlation…" /></label>
        <label className="select-field"><Filter size={16} aria-hidden="true" /><span className="sr-only">Filter audit category</span><select value={category} onChange={(event) => setCategory(event.target.value as AuditEntry["category"] | "all")}><option value="all">All categories</option>{categories.map((item) => <option key={item} value={item}>{sentenceCase(item)}</option>)}</select></label>
      </div>
      {entries.length === 0 ? <EmptyState title="No matching audit records" description="Change the search or category filter. Core does not hide denied or failed decisions." /> : (
        <Panel className="table-panel audit-table-panel">
          <div className="table-scroll"><table>
            <caption className="sr-only">Veqri security and activity audit entries</caption>
            <thead><tr><th scope="col">Time</th><th scope="col">Action</th><th scope="col">Actor → target</th><th scope="col">Decision</th><th scope="col">Summary</th><th scope="col">Correlation</th></tr></thead>
            <tbody>{entries.map((entry) => <tr key={entry.id}>
              <td><time dateTime={entry.occurred_at}>{formatTime(entry.occurred_at)}</time><small>{formatDate(entry.occurred_at).split(",")[0]}</small></td>
              <td><strong>{entry.action}</strong><small>{entry.category}</small></td>
              <td><span>{entry.actor}</span><small>→ {entry.target}</small></td>
              <td><Badge value={entry.decision} /></td>
              <td>{entry.summary}{entry.redacted ? <span className="redacted-label"><LockKeyhole size={12} />redacted</span> : null}</td>
              <td><code>{entry.correlation_id}</code></td>
            </tr>)}</tbody>
          </table></div>
        </Panel>
      )}
    </>
  );
}

export function SettingsPage() {
  const { snapshot, busyAction, runAction } = useDesktop();
  const [draft, setDraft] = useState<DesktopSettings | null>(snapshot?.settings ?? null);
  useEffect(() => { if (snapshot) setDraft(snapshot.settings); }, [snapshot]);
  if (!snapshot || !draft) return null;
  const changed = draft.theme !== snapshot.settings.theme;
  const loopbackOnly = /^(127(?:\.\d{1,3}){3}|localhost|\[::1\]):\d+$/i.test(snapshot.core.bind_address);
  const save = () => runAction({ type: "settings.update", patch: { theme: draft.theme } }, "save theme");
  return (
    <>
      <PageHeader
        eyebrow="Desktop preferences"
        title="Settings"
        description="Theme changes apply here. Retention is enforced by Core and shown read-only; remaining native-shell preferences are not yet applied."
        actions={<div className="page-actions">
          <Button disabled={!changed || busyAction !== null} onClick={() => setDraft(snapshot.settings)}>Discard</Button>
          <Button variant="primary" disabled={!changed || busyAction !== null} onClick={() => void save()}>Save theme</Button>
        </div>}
      />
      <div className="settings-grid">
        <Panel title="Appearance & desktop" description="Theme is active; native lifecycle controls are not yet wired">
          <label className="field"><span>Color theme</span><select value={draft.theme} onChange={(event) => setDraft({ ...draft, theme: event.target.value as DesktopSettings["theme"] })}><option value="dark">Dark</option><option value="light">Light</option><option value="system">Use system</option></select></label>
          <Toggle checked={draft.start_at_login} onChange={() => undefined} disabled label="Start Veqri at login (not applied)" description="Install an OS background service or launch agent, then restart it." />
          <Toggle checked={draft.close_to_tray} onChange={() => undefined} disabled label="Close to tray (not applied)" description="Not available in the checked-in native shell; closing the window may stop the companion." />
          <Toggle checked={draft.desktop_notifications} onChange={() => undefined} disabled label="Desktop notifications (not applied)" description="The optional OS notification bridge is not wired in this build." />
        </Panel>
        <Panel title="Privacy & retention" description="Core enforces this read-only runtime policy on startup and every six hours">
          <label className="field"><span>Transcript and task-content retention</span><span className="input-with-suffix"><input type="number" value={draft.transcript_retention_days} disabled readOnly /><small>days</small></span><small>Set <code>VEQRI_RETENTION_DAYS</code> and restart Core. Records strictly older than the rolling UTC cutoff are scrubbed; 0 retains them indefinitely.</small></label>
          <label className="field"><span>Audit retention</span><span className="input-with-suffix"><input type="number" value={draft.audit_retention_days} disabled readOnly /><small>days</small></span><small>Audit rows use the same Core cutoff. Audit records tied to active or unresolved task work are retained until that work becomes safe to expire.</small></label>
          <Toggle checked={draft.redact_diagnostics} onChange={() => undefined} disabled label="Redact diagnostic exports by default (not applied)" description="Choose the explicit redacted export action on Diagnostics." />
        </Panel>
        <Panel title="Results & quiet hours" description="Notification scheduling is not yet enforced">
          <Toggle checked={draft.announce_background_results} onChange={() => undefined} disabled label="Announce background results (not applied)" description="Unavailable until queued speech policy is connected to this preference." />
          <Toggle checked={draft.quiet_hours_enabled} onChange={() => undefined} disabled label="Quiet hours (not applied)" description="Calls and notifications are not suppressed by these values." />
          <div className="time-grid"><label className="field"><span>Starts</span><input type="time" value={draft.quiet_hours_start} disabled readOnly /></label><label className="field"><span>Ends</span><input type="time" value={draft.quiet_hours_end} disabled readOnly /></label></div>
        </Panel>
        <Panel title="Network exposure" description="Runtime configuration is authoritative">
          <div className={`network-warning ${loopbackOnly ? "" : "is-enabled"}`}><Network size={21} aria-hidden="true" /><div><strong>{loopbackOnly ? "Core is bound to loopback" : "Core has a non-loopback bind"}</strong><p>Current runtime bind: <code>{snapshot.core.bind_address}</code>. Change <code>VEQRI_ADDR</code> with TLS and firewall configuration, then restart Core.</p></div></div>
          <Toggle checked={!loopbackOnly} onChange={() => undefined} disabled label="Non-loopback listener active" description="Read-only runtime status. This desktop screen cannot change network exposure." />
        </Panel>
      </div>
      {changed ? <div className="unsaved-banner" role="status"><FileClock size={17} aria-hidden="true" />You have an unsaved theme change.</div> : null}
    </>
  );
}

export function DiagnosticsPage() {
  const { snapshot, busyAction, runAction, refresh } = useDesktop();
  if (!snapshot) return null;
  const diagnostics = snapshot.diagnostics;
  const unhealthy = diagnostics.checks.filter((check) => check.status !== "healthy").length;
  return (
    <>
      <PageHeader
        eyebrow="Recovery & support"
        title="Diagnostics & backup"
		description="SQLite integrity, simulated voice state, event subscribers, durable storage, consistent local backups, and support exports."
        actions={<Button icon={<RefreshCw size={15} />} onClick={() => void refresh()}>Run checks again</Button>}
      />
      <div className="metric-grid metric-grid--three">
        <MetricCard label="Health checks" value={`${diagnostics.checks.length - unhealthy} / ${diagnostics.checks.length}`} detail={unhealthy ? `${unhealthy} check needs attention` : "All checks healthy"} icon={<Stethoscope size={20} />} tone={unhealthy ? "warning" : "positive"} />
        <MetricCard label="Free storage" value={formatBytes(diagnostics.storage.free_bytes)} detail={`${diagnostics.storage.backup_count} local backups`} icon={<HardDrive size={20} />} />
        <MetricCard label="Event stream" value={diagnostics.event_stream.connected_clients} detail={`${diagnostics.event_stream.backlog} events in backlog`} icon={<Server size={20} />} tone={diagnostics.event_stream.backlog ? "warning" : "positive"} />
      </div>
      <div className="two-column">
        <Panel title="System checks" description={`Generated ${formatDate(diagnostics.generated_at)}`}>
          <div className="check-list">{diagnostics.checks.map((check) => <div key={check.id}><span className={`check-icon check-icon--${check.status}`}>{check.status === "healthy" ? <CheckCircle2 size={17} /> : <TriangleAlert size={17} />}</span><span><strong>{check.name}</strong><small>{check.detail}</small></span><Badge value={check.status} /></div>)}</div>
        </Panel>
		<Panel title="Runtime diagnostics" description="Subscriber and simulated voice state without raw audio or transcript content">
          <DefinitionList items={[
            { term: "Connected UI clients", detail: diagnostics.event_stream.connected_clients },
            { term: "Last durable event", detail: <code>{diagnostics.event_stream.last_event_id}</code> },
            { term: "Event backlog", detail: diagnostics.event_stream.backlog },
			{ term: "Active voice sessions", detail: diagnostics.webrtc.active_peers },
            { term: "STUN", detail: diagnostics.webrtc.stun },
            { term: "TURN", detail: diagnostics.webrtc.turn },
          ]} />
        </Panel>
      </div>
      <Panel title="Backup & export" description="Backups are plain SQLite files. Protect them with operator-managed disk or backup encryption; diagnostic bundles redact sensitive values by default">
        <div className="backup-grid">
          <div className="backup-summary"><span className="entity-icon"><Database size={21} /></span><div><strong>Last backup</strong><p>{formatDate(diagnostics.storage.last_backup_at)}</p><code>{diagnostics.storage.last_backup_path ?? "No backup yet"}</code></div></div>
          <div className="backup-actions">
            <Button variant="primary" icon={<Archive size={16} />} disabled={busyAction !== null} onClick={() => void runAction({ type: "backup.create" }, "create backup")}>Create local backup</Button>
            <Button icon={<FileDown size={16} />} disabled={busyAction !== null} onClick={() => void runAction({ type: "diagnostics.export", redact: true }, "export redacted diagnostics")}>Export redacted bundle</Button>
            <ConfirmButton label="Export full bundle" title="Export diagnostics without redaction?" description="The bundle may contain private paths, local identifiers, task summaries, and message metadata. Authentication tokens and raw secrets are still never exported." confirmLabel="Export unredacted bundle" disabled={busyAction !== null} onConfirm={() => runAction({ type: "diagnostics.export", redact: false }, "export full diagnostics")} />
          </div>
        </div>
      </Panel>
	  <Panel title="Recent redacted logs" description="No in-memory log collector is wired in this build; service logs remain in the configured process supervisor" className="log-panel">
        <div className="log-list" role="log" aria-label="Recent Veqri logs">{diagnostics.recent_logs.map((log) => <div key={log.id} className={`log-row log-row--${log.level.toLowerCase()}`}><time dateTime={log.occurred_at}>{formatTime(log.occurred_at)}</time><Badge value={log.level} /><code>{log.component}</code><span>{log.message}</span><small>{log.correlation_id ?? "system"}</small></div>)}</div>
      </Panel>
    </>
  );
}
