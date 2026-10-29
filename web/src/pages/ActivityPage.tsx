import { useCallback, useEffect, useState } from 'react';
import {
  ChevronDown,
  ChevronRight,
  ExternalLink,
  RefreshCw,
  RotateCcw,
  Search,
} from 'lucide-react';
import { api, shortImageName } from '../api';
import { eventStatusTone, Modal, StatusBadge } from '../components/UI';
import { formatRelativeTime, useI18n } from '../i18n';
import {
  EventKindIcon,
  labelForKind,
  labelForStatus,
  ResultList,
} from './HomePage';
import type { FerryEvent } from '../types';

const EVENT_KINDS = ['all', 'deploy', 'rollout', 'rollback', 'build', 'snapshot', 'pause'] as const;

export function ActivityPage() {
  const { t, locale } = useI18n();

  const [events, setEvents] = useState<FerryEvent[]>([]);
  const [kindFilter, setKindFilter] = useState<string>('all');
  const [imageQuery, setImageQuery] = useState<string>('');
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(false);

  const [rollbackTarget, setRollbackTarget] = useState<FerryEvent | null>(null);
  const [rollingBack, setRollingBack] = useState(false);
  const [errMsg, setErrMsg] = useState('');

  const fetchEvents = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.events({
        kind: kindFilter === 'all' ? undefined : kindFilter,
        image: imageQuery.trim() || undefined,
        limit: 120,
      });
      setEvents(data);
    } finally {
      setLoading(false);
    }
  }, [kindFilter, imageQuery]);

  useEffect(() => {
    void fetchEvents();
  }, [fetchEvents]);

  // Poll every 15 seconds while tab is visible
  useEffect(() => {
    const id = window.setInterval(() => {
      if (document.visibilityState === 'visible') {
        void fetchEvents();
      }
    }, 15000);
    const onVisible = () => {
      if (document.visibilityState === 'visible') {
        void fetchEvents();
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(id);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [fetchEvents]);

  const handleConfirmRollback = async () => {
    if (!rollbackTarget) return;
    setRollingBack(true);
    setErrMsg('');
    try {
      const rbEv = await api.rollback(rollbackTarget.id);
      setRollbackTarget(null);
      setExpanded((prev) => ({ ...prev, [rbEv.id]: true }));
      await fetchEvents();
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    } finally {
      setRollingBack(false);
    }
  };

  return (
    <div className="page-container">
      <header className="page-header">
        <div>
          <h1 className="serif-heading page-title">{t.activityTitle}</h1>
          <p className="page-subtitle">{t.activitySubtitle}</p>
        </div>
        <button
          type="button"
          className="btn btn-subtle"
          onClick={() => fetchEvents()}
          disabled={loading}
        >
          <RefreshCw size={15} className={loading ? 'spin' : ''} />
          <span>{t.refresh}</span>
        </button>
      </header>

      <div className="filter-bar">
        <div className="pill-group wrap">
          {EVENT_KINDS.map((k) => (
            <button
              key={k}
              type="button"
              className={`pill-btn ${kindFilter === k ? 'active' : ''}`}
              onClick={() => setKindFilter(k)}
            >
              {k === 'all' ? t.allKinds : labelForKind(k, t)}
            </button>
          ))}
        </div>
        <div className="search-box">
          <Search size={15} className="text-faint" />
          <input
            type="text"
            value={imageQuery}
            onChange={(e) => setImageQuery(e.target.value)}
            placeholder={t.filterByImagePlaceholder}
          />
        </div>
      </div>

      {errMsg && <div className="inline-alert inline-alert-err">{errMsg}</div>}

      {events.length === 0 ? (
        <div className="surface-card empty-state">{t.noActivityYet}</div>
      ) : (
        <div className="stack-sm">
          {events.map((ev) => {
            const hasResults = (ev.results?.length || 0) > 0;
            const isOpen = Boolean(expanded[ev.id]);
            const canRollback =
              (ev.kind === 'deploy' || ev.kind === 'rollout') &&
              (ev.results || []).some((r) => r.result === 'deployed' && r.from && r.to);

            return (
              <article key={ev.id} className="surface-card event-card">
                <div className="event-main">
                  <div className="row-gap-sm min-w-0 wrap">
                    <EventKindIcon kind={ev.kind} />
                    <span className="event-kind-label">{labelForKind(ev.kind, t)}</span>
                    {ev.image && (
                      <span className="mono text-sm truncate" title={ev.image}>
                        {shortImageName(ev.image)}
                        {ev.tag ? `:${ev.tag}` : ''}
                      </span>
                    )}
                    {ev.commit && !ev.tag && (
                      <span className="tag-chip mono">{ev.commit.slice(0, 12)}</span>
                    )}
                    <StatusBadge tone={eventStatusTone(ev.status)}>
                      {labelForStatus(ev.status, t)}
                    </StatusBadge>
                  </div>

                  <div className="row-gap-sm shrink-0">
                    {ev.logUrl && (
                      <a
                        href={ev.logUrl}
                        target="_blank"
                        rel="noreferrer"
                        className="btn btn-subtle btn-sm"
                      >
                        <span>{t.buildLogsLink}</span>
                        <ExternalLink size={13} />
                      </a>
                    )}
                    {canRollback && (
                      <button
                        type="button"
                        className="btn btn-subtle btn-sm"
                        onClick={() => {
                          setErrMsg('');
                          setRollbackTarget(ev);
                        }}
                      >
                        <RotateCcw size={13} />
                        <span>{t.rollbackBtn}</span>
                      </button>
                    )}
                  </div>
                </div>

                <div className="event-subrow text-xs text-muted">
                  <div className="row-gap-sm wrap">
                    <span className="mono">{ev.actor}</span>
                    <span>·</span>
                    <span title={ev.at}>{formatRelativeTime(ev.at, locale)}</span>
                    {ev.source && (
                      <>
                        <span>·</span>
                        <span className="mono">{ev.source}</span>
                      </>
                    )}
                    {ev.message && (
                      <>
                        <span>·</span>
                        <span>{ev.message}</span>
                      </>
                    )}
                  </div>

                  {hasResults && (
                    <button
                      type="button"
                      className="link-btn row-gap-xs text-xs"
                      onClick={() =>
                        setExpanded((prev) => ({ ...prev, [ev.id]: !isOpen }))
                      }
                    >
                      {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                      <span>
                        {isOpen ? t.detailsHide : t.detailsShow} ({ev.results!.length})
                      </span>
                    </button>
                  )}
                </div>

                {hasResults && isOpen && (
                  <div className="event-results-wrap">
                    <ResultList results={ev.results!} t={t} />
                  </div>
                )}
              </article>
            );
          })}
        </div>
      )}

      <Modal
        open={rollbackTarget !== null}
        title={t.rollbackModalTitle}
        onClose={() => setRollbackTarget(null)}
        width={600}
        footer={
          <>
            <button
              type="button"
              className="btn btn-subtle"
              onClick={() => setRollbackTarget(null)}
            >
              {t.cancel}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              disabled={rollingBack}
              onClick={handleConfirmRollback}
            >
              {rollingBack ? t.deploying : t.confirmRollbackBtn}
            </button>
          </>
        }
      >
        {rollbackTarget && (
          <div className="stack-sm">
            <p className="text-sm text-muted">{t.rollbackModalDesc}</p>
            <div className="result-table">
              {(rollbackTarget.results || [])
                .filter((r) => r.result === 'deployed')
                .map((r, idx) => (
                  <div key={idx} className="result-row">
                    <div className="row-gap-sm">
                      <span className="cluster-pill mono">{r.cluster}</span>
                      <span className="text-xs text-muted">{r.namespace}</span>
                      <span className="workload-ref">
                        <span className="text-faint text-xs">{r.kind}/</span>
                        {r.name}
                      </span>
                    </div>
                    <span className="mono text-xs">
                      {r.to.split(':').pop()} →{' '}
                      <strong className="text-strong">{r.from.split(':').pop()}</strong>
                    </span>
                  </div>
                ))}
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}
