import {
  Activity,
  Bot,
  BrainCircuit,
  Cable,
  CheckSquare,
  ChevronLeft,
  CircleGauge,
  ClipboardList,
  DatabaseBackup,
  FileClock,
  HeartPulse,
  Menu,
  MessageSquareText,
  Mic2,
  RefreshCw,
  Settings,
  ShieldCheck,
  Smartphone,
  Stethoscope,
  Unplug,
  X,
  Zap,
} from "lucide-react";
import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { useDesktop } from "../state/DesktopContext";
import { Button, IconButton, LoadStatePanel } from "./ui";

const navGroups = [
  {
    label: "Overview",
    links: [
      { to: "/", label: "Dashboard", icon: CircleGauge, end: true },
      { to: "/core", label: "Core health", icon: HeartPulse },
    ],
  },
  {
    label: "Activity",
    links: [
      { to: "/devices", label: "Devices", icon: Smartphone },
      { to: "/voice", label: "Voice sessions", icon: Mic2 },
      { to: "/conversations", label: "Conversations", icon: MessageSquareText },
      { to: "/tasks", label: "Tasks", icon: ClipboardList },
      { to: "/approvals", label: "Approvals", icon: CheckSquare },
    ],
  },
  {
    label: "Runtime",
    links: [
      { to: "/agents", label: "Agents", icon: Bot },
      { to: "/tools", label: "Tools & policies", icon: ShieldCheck },
      { to: "/connectors", label: "Connectors", icon: Cable },
      { to: "/providers", label: "Providers", icon: BrainCircuit },
    ],
  },
  {
    label: "System",
    links: [
      { to: "/audit", label: "Audit log", icon: FileClock },
      { to: "/settings", label: "Settings", icon: Settings },
      { to: "/diagnostics", label: "Diagnostics & backup", icon: Stethoscope },
    ],
  },
] as const;

const titles: Record<string, string> = {
  "/": "Dashboard",
  "/core": "Core health",
  "/devices": "Android devices",
  "/voice": "Voice sessions",
  "/conversations": "Conversations",
  "/tasks": "Tasks",
  "/approvals": "Pending approvals",
  "/agents": "Agent registry",
  "/tools": "Tools & policies",
  "/connectors": "Messaging connectors",
  "/providers": "Voice & AI providers",
  "/audit": "Audit log",
  "/settings": "Settings",
  "/diagnostics": "Diagnostics & backup",
};

export function AppShell() {
  const location = useLocation();
  const [navOpen, setNavOpen] = useState(false);
  const { snapshot, loadState, streamState, retryAttempt, error, gatewayMode, endpoint, busyAction, notice, refresh, dismissNotice } = useDesktop();
  const pendingApprovals = snapshot?.approvals.filter((approval) => approval.status === "pending").length ?? 0;
  const title = location.pathname.startsWith("/tasks/") ? "Task graph" : (titles[location.pathname] ?? "Veqri");

  useEffect(() => setNavOpen(false), [location.pathname]);

  const connectionCopy = streamState === "connected"
    ? "Live event stream connected"
    : streamState === "retrying"
      ? `Event stream retrying · attempt ${retryAttempt}`
      : streamState === "connecting"
        ? "Connecting to event stream"
        : streamState === "failed"
          ? "Event stream reconnect failed"
          : "Event stream disconnected";

  return (
    <div className="app-frame">
      <a className="skip-link" href="#main-content">Skip to main content</a>
      <aside className={`sidebar ${navOpen ? "is-open" : ""}`} aria-label="Primary navigation">
        <div className="brand">
          <div className="brand__mark" aria-hidden="true"><Zap size={19} fill="currentColor" /></div>
          <div><strong>Veqri</strong><span>local orchestrator</span></div>
          <IconButton label="Close navigation" className="sidebar__close" onClick={() => setNavOpen(false)}><X size={19} /></IconButton>
        </div>
        <nav className="nav-list">
          {navGroups.map((group) => (
            <div className="nav-group" key={group.label}>
              <p>{group.label}</p>
              {group.links.map(({ to, label, icon: Icon, ...link }) => (
                <NavLink key={to} to={to} end={"end" in link ? link.end : false} className={({ isActive }) => `nav-link ${isActive ? "is-active" : ""}`}>
                  <Icon size={17} aria-hidden="true" />
                  <span>{label}</span>
                  {to === "/approvals" && pendingApprovals > 0 ? <span className="nav-count" aria-label={`${pendingApprovals} pending`}>{pendingApprovals}</span> : null}
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
        <div className="sidebar__footer">
          <div className="sidebar-status">
            <span className={`connection-dot connection-dot--${streamState}`} aria-hidden="true" />
            <div><strong>{gatewayMode === "mock" ? "Mock core" : "Local core"}</strong><span>{endpoint}</span></div>
          </div>
        </div>
      </aside>
      {navOpen ? <button className="sidebar-scrim" aria-label="Close navigation" onClick={() => setNavOpen(false)} /> : null}
      <div className="workspace">
        <header className="topbar">
          <div className="topbar__title">
            <IconButton label="Open navigation" className="menu-button" aria-expanded={navOpen} onClick={() => setNavOpen(true)}><Menu size={19} /></IconButton>
            {location.pathname.startsWith("/tasks/") ? <NavLink className="back-link" to="/tasks"><ChevronLeft size={17} aria-hidden="true" />Tasks</NavLink> : null}
            <span>{title}</span>
          </div>
          <div className="topbar__actions">
            <span className={`stream-pill stream-pill--${streamState}`} role="status">
              {streamState === "connected" ? <Activity size={14} aria-hidden="true" /> : <Unplug size={14} aria-hidden="true" />}
              {connectionCopy}
            </span>
            <Button variant="ghost" icon={<RefreshCw size={15} className={busyAction ? "spin" : ""} />} onClick={() => void refresh()} disabled={loadState === "loading"}>Refresh</Button>
          </div>
        </header>
        {streamState !== "connected" && snapshot ? (
          <div className={`connection-banner connection-banner--${streamState}`} role="status">
            <Unplug size={17} aria-hidden="true" />
            <span><strong>{connectionCopy}.</strong> Showing the last authenticated snapshot; actions may be unavailable.</span>
          </div>
        ) : null}
        {snapshot?.core.emergency_stop ? (
          <div className="emergency-banner" role="alert"><ShieldCheck size={18} aria-hidden="true" /><strong>Emergency stop is active.</strong> New tool executions are blocked by Core.</div>
        ) : null}
        <main id="main-content" className="main-content" tabIndex={-1}>
          {snapshot ? <Outlet /> : <LoadStatePanel state={loadState} error={error} onRetry={() => void refresh()} />}
        </main>
      </div>
      {notice ? (
        <div className={`toast toast--${notice.tone}`} role="status" aria-live="polite">
          <span>{notice.message}</span>
          {notice.artifactPath && window.veqriShell?.revealFile ? (
            <button onClick={() => void window.veqriShell?.revealFile?.(notice.artifactPath!)}>Reveal file</button>
          ) : null}
          <IconButton label="Dismiss notification" onClick={dismissNotice}><X size={16} /></IconButton>
        </div>
      ) : null}
      <span className="sr-only" role="status" aria-live="polite">{busyAction ? `Working on ${busyAction}` : ""}</span>
    </div>
  );
}
