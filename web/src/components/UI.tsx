import { useEffect, useState, type ReactNode } from 'react';
import { Check, Copy, X } from 'lucide-react';
import { useI18n } from '../i18n';

/** Minimal geometric ferry boat & calm wave mark in warm terracotta. */
export function FerryMark({ size = 26 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 28 28"
      fill="none"
      aria-hidden="true"
      className="ferry-mark"
    >
      <rect width="28" height="28" rx="7" fill="var(--accent-soft)" />
      <path
        d="M8.5 13.5L10.2 8.5H17.8L19.5 13.5"
        stroke="var(--accent)"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path
        d="M6.2 14L14 11.4L21.8 14L19.8 18.2H8.2L6.2 14Z"
        fill="var(--accent)"
      />
      <path
        d="M5.5 21.2C7.4 20.1 9.4 20.1 11.2 21.2C13 22.3 15 22.3 16.8 21.2C18.6 20.1 20.6 20.1 22.5 21.2"
        stroke="var(--accent)"
        strokeWidth="1.65"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function StatusBadge({
  tone,
  children,
  title,
}: {
  tone: 'ok' | 'warn' | 'err' | 'neutral' | 'accent';
  children: ReactNode;
  title?: string;
}) {
  return (
    <span className={`badge badge-${tone}`} title={title}>
      {children}
    </span>
  );
}

export function resultTone(result: string): 'ok' | 'warn' | 'err' | 'neutral' | 'accent' {
  switch (result) {
    case 'deployed':
      return 'ok';
    case 'would-deploy':
      return 'accent';
    case 'paused':
    case 'argo-cd':
      return 'warn';
    case 'error':
      return 'err';
    default:
      return 'neutral';
  }
}

export function eventStatusTone(status: string): 'ok' | 'warn' | 'err' | 'neutral' | 'accent' {
  switch (status) {
    case 'ok':
      return 'ok';
    case 'started':
    case 'queued':
      return 'accent';
    case 'partial':
      return 'warn';
    case 'error':
      return 'err';
    default:
      return 'neutral';
  }
}

export function Modal({
  open,
  title,
  onClose,
  children,
  footer,
  width = 520,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="modal-backdrop" onMouseDown={onClose} role="presentation">
      <div
        className="modal-card"
        style={{ maxWidth: width }}
        onMouseDown={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <div className="modal-header">
          <h2 className="modal-title">{title}</h2>
          <button type="button" className="icon-btn" onClick={onClose} aria-label="Close">
            <X size={18} strokeWidth={1.7} />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-footer">{footer}</div>}
      </div>
    </div>
  );
}

export function CopyBlock({ code, label }: { code: string; label?: string }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Fallback ignored
    }
  };

  return (
    <div className="copy-block">
      {label && <div className="copy-block-label">{label}</div>}
      <div className="copy-block-inner">
        <pre>
          <code>{code}</code>
        </pre>
        <button type="button" className="btn btn-subtle btn-sm copy-btn" onClick={handleCopy}>
          {copied ? <Check size={14} strokeWidth={1.8} /> : <Copy size={14} strokeWidth={1.8} />}
          <span>{copied ? t.copied : t.copy}</span>
        </button>
      </div>
    </div>
  );
}
