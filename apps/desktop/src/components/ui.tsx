import { AlertCircle, Box, LoaderCircle, RotateCw, X } from "lucide-react";
import { type ButtonHTMLAttributes, type PropsWithChildren, type ReactNode, type Ref, useEffect, useId, useRef, useState } from "react";
import type { LoadState } from "../api/types";
import { statusTone } from "../lib/format";

export function PageHeader({ eyebrow, title, description, actions }: { eyebrow: string; title: string; description: string; actions?: ReactNode }) {
  return (
    <header className="page-header">
      <div>
        <p className="eyebrow">{eyebrow}</p>
        <h1>{title}</h1>
        <p className="page-description">{description}</p>
      </div>
      {actions ? <div className="page-actions">{actions}</div> : null}
    </header>
  );
}

export function Badge({ value, label }: { value: string; label?: string }) {
  return (
    <span className={`badge badge--${statusTone(value)}`} aria-label={label ?? value}>
      <span className="badge__dot" aria-hidden="true" />
      {value.replaceAll("_", " ")}
    </span>
  );
}

export function Button({ variant = "secondary", icon, children, ref, className = "", ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "primary" | "secondary" | "danger" | "ghost"; icon?: ReactNode; ref?: Ref<HTMLButtonElement> }) {
  return (
    <button ref={ref} className={`button button--${variant} ${className}`.trim()} {...props}>
      {icon ? <span className="button__icon" aria-hidden="true">{icon}</span> : null}
      <span>{children}</span>
    </button>
  );
}

export function IconButton({ label, children, className = "", ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return (
    <button className={`icon-button ${className}`.trim()} aria-label={label} title={label} {...props}>
      {children}
    </button>
  );
}

export function Panel({ title, description, action, className = "", children }: PropsWithChildren<{ title?: string; description?: string; action?: ReactNode; className?: string }>) {
  return (
    <section className={`panel ${className}`}>
      {title || action ? (
        <header className="panel__header">
          <div>
            {title ? <h2>{title}</h2> : null}
            {description ? <p>{description}</p> : null}
          </div>
          {action}
        </header>
      ) : null}
      {children}
    </section>
  );
}

export function MetricCard({ label, value, detail, icon, tone = "default" }: { label: string; value: ReactNode; detail: string; icon: ReactNode; tone?: "default" | "positive" | "warning" | "danger" }) {
  return (
    <article className={`metric-card metric-card--${tone}`}>
      <div className="metric-card__icon" aria-hidden="true">{icon}</div>
      <div>
        <p className="metric-card__label">{label}</p>
        <p className="metric-card__value">{value}</p>
        <p className="metric-card__detail">{detail}</p>
      </div>
    </article>
  );
}

export function Progress({ value, label }: { value: number; label: string }) {
  const clamped = Math.max(0, Math.min(100, value));
  return (
    <div className="progress-wrap">
      <div className="progress-meta"><span>{label}</span><span>{clamped}%</span></div>
      <div className="progress" role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={clamped}>
        <span style={{ width: `${clamped}%` }} />
      </div>
    </div>
  );
}

export function EmptyState({ title, description, action }: { title: string; description: string; action?: ReactNode }) {
  return (
    <div className="empty-state">
      <span className="empty-state__icon" aria-hidden="true"><Box size={24} /></span>
      <h2>{title}</h2>
      <p>{description}</p>
      {action}
    </div>
  );
}

export function LoadStatePanel({ state, error, onRetry }: { state: LoadState; error: string | null; onRetry: () => void }) {
  const content: Record<Exclude<LoadState, "ready" | "empty">, { title: string; description: string }> = {
    loading: { title: "Loading local state", description: "Opening an authenticated connection to Veqri Core…" },
    disconnected: { title: "Core is disconnected", description: error ?? "The localhost core cannot be reached. Existing tasks continue in the core when it returns." },
    retrying: { title: "Reconnecting to Core", description: "The event stream is retrying with bounded backoff." },
    failed: { title: "Desktop state failed to load", description: error ?? "Veqri Core returned an unexpected response." },
  };
  if (state === "ready") return null;
  if (state === "empty") return <EmptyState title="Nothing here yet" description="This view has no records to show." />;
  const stateContent = content[state];
  return (
    <div className="state-panel" role={state === "loading" ? "status" : "alert"}>
      <span className={`state-panel__icon ${state === "loading" || state === "retrying" ? "is-spinning" : ""}`} aria-hidden="true">
        {state === "loading" || state === "retrying" ? <LoaderCircle size={26} /> : <AlertCircle size={26} />}
      </span>
      <h1>{stateContent.title}</h1>
      <p>{stateContent.description}</p>
      {state !== "loading" ? <Button onClick={onRetry} icon={<RotateCw size={15} />}>Retry now</Button> : null}
    </div>
  );
}

export function DefinitionList({ items }: { items: Array<{ term: string; detail: ReactNode }> }) {
  return (
    <dl className="definition-list">
      {items.map(({ term, detail }) => (
        <div key={term}>
          <dt>{term}</dt>
          <dd>{detail}</dd>
        </div>
      ))}
    </dl>
  );
}

export function ConfirmButton({
  label,
  title,
  description,
  confirmLabel,
  onConfirm,
  danger = true,
  disabled,
}: {
  label: string;
  title: string;
  description: string;
  confirmLabel: string;
  onConfirm: () => unknown | Promise<unknown>;
  danger?: boolean;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const titleId = useId();
  const descriptionId = useId();
  const triggerRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const restoreFocus = useRef(false);
  const closeDialog = () => {
    restoreFocus.current = true;
    setOpen(false);
  };
  useEffect(() => {
    if (!open) {
      if (restoreFocus.current) {
        restoreFocus.current = false;
        triggerRef.current?.focus();
      }
      return;
    }
    confirmRef.current?.focus();
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") closeDialog();
    };
    window.addEventListener("keydown", escape);
    return () => window.removeEventListener("keydown", escape);
  }, [open]);

  return (
    <>
      <Button ref={triggerRef} variant={danger ? "danger" : "secondary"} disabled={disabled} onClick={() => setOpen(true)}>{label}</Button>
      {open ? (
        <div className="modal-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget) closeDialog(); }}>
          <section className="confirm-dialog" role="alertdialog" aria-modal="true" aria-labelledby={titleId} aria-describedby={descriptionId}>
            <div className="confirm-dialog__title-row">
              <h2 id={titleId}>{title}</h2>
              <IconButton label="Close confirmation" onClick={closeDialog}><X size={18} /></IconButton>
            </div>
            <p id={descriptionId}>{description}</p>
            <div className="confirm-dialog__actions">
              <Button onClick={closeDialog}>Keep current state</Button>
              <Button
                ref={confirmRef}
                variant={danger ? "danger" : "primary"}
                onClick={() => {
                  closeDialog();
                  void onConfirm();
                }}
              >
                {confirmLabel}
              </Button>
            </div>
          </section>
        </div>
      ) : null}
    </>
  );
}

export function Toggle({ checked, onChange, label, description, disabled }: { checked: boolean; onChange: (checked: boolean) => void; label: string; description?: string; disabled?: boolean }) {
  return (
    <label className={`toggle-row ${disabled ? "is-disabled" : ""}`}>
      <span><strong>{label}</strong>{description ? <small>{description}</small> : null}</span>
      <input type="checkbox" role="switch" checked={checked} disabled={disabled} onChange={(event) => onChange(event.target.checked)} />
    </label>
  );
}
