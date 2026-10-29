import { useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router';
import {
  ArrowRight,
  ArrowUp,
  CheckCircle2,
  GitBranch,
  History,
  Layers,
  PauseCircle,
  RotateCcw,
  Server,
  Sparkles,
} from 'lucide-react';
import { api, parseComposerInput, shortImageName } from '../api';
import {
  eventStatusTone,
  FerryMark,
  resultTone,
  StatusBadge,
} from '../components/UI';
import { formatRelativeTime, greetingForNow, useI18n, type Messages } from '../i18n';
import type {
  DeployResult,
  FerryEvent,
  InventoryResponse,
  MeResponse,
} from '../types';

export function HomePage({
  me,
  focusTrigger,
}: {
  me: MeResponse;
  focusTrigger: number;
}) {
  const { t, locale } = useI18n();
  const inputRef = useRef<HTMLInputElement>(null);

  const [inv, setInv] = useState<InventoryResponse | null>(null);
  const [events, setEvents] = useState<FerryEvent[]>([]);
  const [input, setInput] = useState('');
  const [sourceNote, setSourceNote] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState<'preview' | 'deploy' | null>(null);
  const [previewTarget, setPreviewTarget] = useState<{ image: string; tag: string } | null>(null);
  const [previewResults, setPreviewResults] = useState<DeployResult[] | null>(null);
  const [deployResults, setDeployResults] = useState<DeployResult[] | null>(null);

  const loadData = async (fresh = false) => {
    try {
      const [w, evs] = await Promise.all([
        api.workloads(fresh),
        api.events({ limit: 10 }),
      ]);
      setInv(w);
      setEvents(evs);
    } catch {
      // Handled gracefully by empty/skeleton state
    }
  };

  useEffect(() => {
    void loadData(false);
  }, []);

  useEffect(() => {
    if (focusTrigger > 0) {
      inputRef.current?.focus();
    }
  }, [focusTrigger]);

  // Dismiss dry-run preview with Escape
  useEffect(() => {
    if (!previewResults && !deployResults) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setPreviewResults(null);
        setDeployResults(null);
        setPreviewTarget(null);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [previewResults, deployResults]);

  // Compute 3 recent unique image repositories for one-click chips
  const recentImageRepos = useMemo(() => {
    const seen = new Set<string>();
    const list: string[] = [];
    for (const ev of events) {
      if (ev.image && !seen.has(ev.image)) {
        seen.add(ev.image);
        list.push(ev.image);
        if (list.length >= 3) return list;
      }
    }
    for (const w of inv?.workloads || []) {
      for (const c of w.containers) {
        if (c.repo && !seen.has(c.repo)) {
          seen.add(c.repo);
          list.push(c.repo);
          if (list.length >= 3) return list;
        }
      }
    }
    return list;
  }, [events, inv]);

  const handlePreview = async (e?: React.FormEvent) => {
    e?.preventDefault();
    setError('');
    setDeployResults(null);
    const parsed = parseComposerInput(input);
    if (!parsed) {
      setError(t.composerInvalid);
      return;
    }
    setBusy('preview');
    try {
      const res = await api.previewDeploy(parsed.image, parsed.tag, sourceNote.trim());
      setPreviewTarget(parsed);
      setPreviewResults(res);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  };

  const handleConfirmDeploy = async () => {
    if (!previewTarget) return;
    setError('');
    setBusy('deploy');
    try {
      const res = await api.deploy(previewTarget.image, previewTarget.tag, sourceNote.trim());
      setPreviewResults(null);
      setDeployResults(res);
      await loadData(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  };

  const handleChipClick = (repo: string) => {
    setInput(`${repo}:`);
    setPreviewResults(null);
    setDeployResults(null);
    setError('');
    requestAnimationFrame(() => {
      const el = inputRef.current;
      if (el) {
        el.focus();
        el.setSelectionRange(el.value.length, el.value.length);
      }
    });
  };

  const wouldDeployCount = useMemo(
    () => (previewResults || []).filter((r) => r.result === 'would-deploy').length,
    [previewResults],
  );

  return (
    <div className="page-container page-home">
      <header className="home-hero">
        <div className="home-greeting-row">
          <FerryMark size={34} />
          <h1 className="serif-heading home-greeting">
            {greetingForNow(t, me.name)}
          </h1>
        </div>
        <p className="home-subtitle">{t.homeSubtitle}</p>
      </header>

      <section className="composer-shell">
        <form onSubmit={handlePreview} className="composer-card">
          <div className="composer-main-row">
            <input
              ref={inputRef}
              type="text"
              className="composer-input mono"
              value={input}
              onChange={(e) => {
                setInput(e.target.value);
                if (previewResults) setPreviewResults(null);
              }}
              placeholder={t.composerPlaceholder}
              spellCheck={false}
              autoComplete="off"
            />
            <button
              type="submit"
              className="composer-submit"
              disabled={!input.trim() || busy !== null}
              title={t.previewBtn}
            >
              <ArrowUp size={18} strokeWidth={2} />
            </button>
          </div>
          <div className="composer-footer-row">
            <input
              type="text"
              className="composer-note-input"
              value={sourceNote}
              onChange={(e) => setSourceNote(e.target.value)}
              placeholder={t.composerSourcePlaceholder}
              spellCheck={false}
            />
            <span className="composer-hint">{t.composerHint}</span>
          </div>
        </form>

        {error && <div className="inline-alert inline-alert-err">{error}</div>}

        {recentImageRepos.length > 0 && !previewResults && !deployResults && (
          <div className="recent-chips-row">
            <span className="recent-chips-label">{t.recentImages}:</span>
            {recentImageRepos.map((repo) => (
              <button
                key={repo}
                type="button"
                className="chip mono"
                onClick={() => handleChipClick(repo)}
                title={repo}
              >
                {shortImageName(repo)}
              </button>
            ))}
          </div>
        )}

        {previewResults && previewTarget && (
          <div className="surface-card preview-panel">
            <div className="preview-header">
              <div>
                <h2 className="section-title">{t.previewTitle}</h2>
                <div className="mono text-muted text-sm">
                  {previewTarget.image}:{previewTarget.tag}
                </div>
              </div>
              <div className="preview-actions">
                <button
                  type="button"
                  className="btn btn-subtle"
                  onClick={() => {
                    setPreviewResults(null);
                    setPreviewTarget(null);
                  }}
                >
                  {t.cancel}
                </button>
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={wouldDeployCount === 0 || busy === 'deploy'}
                  onClick={handleConfirmDeploy}
                >
                  {busy === 'deploy'
                    ? t.deploying
                    : `${t.deployNowBtn}${wouldDeployCount > 0 ? ` (${wouldDeployCount})` : ''}`}
                </button>
              </div>
            </div>

            {previewResults.length === 0 ? (
              <p className="empty-inline">{t.noMatchingWorkloads}</p>
            ) : (
              <ResultList results={previewResults} t={t} />
            )}
          </div>
        )}

        {deployResults && (
          <div className="surface-card preview-panel">
            <div className="preview-header">
              <div className="row-gap-sm">
                <CheckCircle2 size={18} strokeWidth={1.8} className="text-ok" />
                <h2 className="section-title">{t.deployCompleteTitle}</h2>
              </div>
              <div className="preview-actions">
                <Link to="/activity" className="btn btn-subtle btn-sm">
                  <span>{t.viewInActivity}</span>
                  <ArrowRight size={14} />
                </Link>
                <button
                  type="button"
                  className="btn btn-subtle btn-sm"
                  onClick={() => setDeployResults(null)}
                >
                  {t.close}
                </button>
              </div>
            </div>
            <ResultList results={deployResults} t={t} />
          </div>
        )}
      </section>

      <div className="home-grid">
        <section className="surface-card">
          <div className="card-header">
            <h2 className="section-title">{t.clustersCardTitle}</h2>
            <Link to="/services" className="subtle-link">
              {t.viewAll}
            </Link>
          </div>
          <div className="cluster-list">
            {(inv?.clusters || []).map((cl) => (
              <div key={cl.name} className="cluster-row">
                <div className="row-gap-sm">
                  <span
                    className={`status-dot ${
                      !cl.reachable
                        ? 'dot-err'
                        : cl.notReadyCount > 0
                          ? 'dot-warn'
                          : 'dot-ok'
                    }`}
                  />
                  <span className="cluster-name mono">{cl.name}</span>
                  {cl.version && (
                    <span className="text-faint mono text-xs">{cl.version}</span>
                  )}
                </div>
                <div className="row-gap-sm text-sm text-muted">
                  {cl.reachable ? (
                    <>
                      <span>
                        {cl.workloadCount} {t.workloadsCount}
                      </span>
                      {cl.notReadyCount > 0 && (
                        <StatusBadge tone="warn">
                          {cl.notReadyCount} {t.notReadyCount}
                        </StatusBadge>
                      )}
                    </>
                  ) : (
                    <StatusBadge tone="err">{t.clusterUnreachable}</StatusBadge>
                  )}
                </div>
              </div>
            ))}
          </div>
        </section>

        <section className="surface-card">
          <div className="card-header">
            <h2 className="section-title">{t.recentActivityTitle}</h2>
            <Link to="/activity" className="subtle-link">
              {t.viewAll}
            </Link>
          </div>
          {events.length === 0 ? (
            <p className="empty-inline">{t.noActivityYet}</p>
          ) : (
            <div className="compact-activity-list">
              {events.map((ev) => (
                <Link key={ev.id} to="/activity" className="compact-activity-row">
                  <div className="row-gap-sm min-w-0">
                    <EventKindIcon kind={ev.kind} />
                    <span className="truncate mono text-sm">
                      {ev.image
                        ? `${shortImageName(ev.image)}${ev.tag ? `:${ev.tag}` : ''}`
                        : ev.message || labelForKind(ev.kind, t)}
                    </span>
                  </div>
                  <div className="row-gap-sm shrink-0">
                    <StatusBadge tone={eventStatusTone(ev.status)}>
                      {labelForStatus(ev.status, t)}
                    </StatusBadge>
                    <span className="text-xs text-faint">
                      {formatRelativeTime(ev.at, locale)}
                    </span>
                  </div>
                </Link>
              ))}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

export function ResultList({ results, t }: { results: DeployResult[]; t: Messages }) {
  return (
    <div className="result-table">
      {results.map((r, idx) => {
        const fromTag = r.from ? r.from.split(':').pop() : '—';
        const toTag = r.to ? r.to.split(':').pop() : '—';
        return (
          <div
            key={`${r.cluster}-${r.namespace}-${r.kind}-${r.name}-${r.container}-${idx}`}
            className="result-row"
          >
            <div className="row-gap-sm min-w-0">
              <span className="cluster-pill mono">{r.cluster}</span>
              <span className="text-muted text-xs">{r.namespace}</span>
              <span className="workload-ref">
                <span className="text-faint text-xs">{r.kind}/</span>
                {r.name}
              </span>
              {r.container && r.container !== r.name && (
                <span className="text-xs text-muted mono">({r.container})</span>
              )}
            </div>
            <div className="row-gap-sm shrink-0">
              {r.from && r.to && (
                <span className="mono text-xs text-muted">
                  {fromTag} → <strong className="text-strong">{toTag}</strong>
                </span>
              )}
              <StatusBadge tone={resultTone(r.result)} title={r.error}>
                {labelForResult(r.result, t)}
              </StatusBadge>
            </div>
          </div>
        );
      })}
    </div>
  );
}

export function EventKindIcon({ kind }: { kind: string }) {
  switch (kind) {
    case 'deploy':
      return <Layers size={15} strokeWidth={1.7} className="text-accent shrink-0" />;
    case 'rollout':
      return <Sparkles size={15} strokeWidth={1.7} className="text-accent shrink-0" />;
    case 'rollback':
      return <RotateCcw size={15} strokeWidth={1.7} className="text-warn shrink-0" />;
    case 'build':
      return <GitBranch size={15} strokeWidth={1.7} className="text-muted shrink-0" />;
    case 'pause':
      return <PauseCircle size={15} strokeWidth={1.7} className="text-muted shrink-0" />;
    case 'snapshot':
      return <Server size={15} strokeWidth={1.7} className="text-muted shrink-0" />;
    default:
      return <History size={15} strokeWidth={1.7} className="text-muted shrink-0" />;
  }
}

export function labelForResult(res: string, t: Messages): string {
  const key = `res_${res}` as keyof Messages;
  return (t[key] as string) || res;
}

export function labelForStatus(st: string, t: Messages): string {
  const key = `st_${st}` as keyof Messages;
  return (t[key] as string) || st;
}

export function labelForKind(kind: string, t: Messages): string {
  const key = `kind_${kind}` as keyof Messages;
  return (t[key] as string) || kind;
}
