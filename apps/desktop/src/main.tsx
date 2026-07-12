import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { HashRouter } from "react-router-dom";
import { AppRoutes } from "./App";
import { DesktopProvider } from "./state/DesktopContext";
import "./styles.css";

const root = document.getElementById("root");
if (!root) throw new Error("Veqri Desktop root element was not found.");

createRoot(root).render(
  <StrictMode>
    <HashRouter>
      <DesktopProvider>
        <AppRoutes />
      </DesktopProvider>
    </HashRouter>
  </StrictMode>,
);
