import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "./components/AppShell";
import {
  ApprovalsPage,
  ConversationsPage,
  DevicesPage,
  TaskDetailsPage,
  TasksPage,
  VoiceSessionsPage,
} from "./pages/ActivityPages";
import { CoreHealthPage, DashboardPage } from "./pages/OverviewPages";
import { AgentsPage, ConnectorsPage, ProvidersPage, ToolsPoliciesPage } from "./pages/RuntimePages";
import { AuditPage, DiagnosticsPage, SettingsPage } from "./pages/SystemPages";

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<DashboardPage />} />
        <Route path="core" element={<CoreHealthPage />} />
        <Route path="devices" element={<DevicesPage />} />
        <Route path="voice" element={<VoiceSessionsPage />} />
        <Route path="conversations" element={<ConversationsPage />} />
        <Route path="tasks" element={<TasksPage />} />
        <Route path="tasks/:taskId" element={<TaskDetailsPage />} />
        <Route path="approvals" element={<ApprovalsPage />} />
        <Route path="agents" element={<AgentsPage />} />
        <Route path="tools" element={<ToolsPoliciesPage />} />
        <Route path="connectors" element={<ConnectorsPage />} />
        <Route path="providers" element={<ProvidersPage />} />
        <Route path="audit" element={<AuditPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="diagnostics" element={<DiagnosticsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
