import { useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import {
  Clock,
  GitBranch,
  MoreHorizontal,
  Pause,
  Play,
  RefreshCw,
  Search,
  Tag,
} from 'lucide-react';
import { api, shortImageName } from '../api';
import { Modal, StatusBadge } from '../components/UI';
import { formatRelativeTime, useI18n } from '../i18n';
import type { InventoryResponse, SourceItem, WorkloadItem } from '../types';

interface GroupedWorkloads {
  repo: string;
  source?: SourceItem;
  tags: string[];
  rows: Array<{
    workload: WorkloadItem;
    containerName: string;
    tag: string;
    init?: boolean;
  }>;
}

export function ServicesPage() {
  const { t, locale } = useI18n();
  const [searchParams, setSearchParams] = useSearchParams();

  const [inv, setInv] = useState<InventoryResponse | null>(null);
  const [sources, setSources] = useState<SourceItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [clusterFilter, setClusterFilter] = useState<string>('all');
  const [query, setQuery] = useState<string>(() => searchParams.get('q') || '');

  const [openMenuKey, setOpenMenuKey] = useState<string | null>(null);
  const [rolloutModal, setRolloutModal] = useState<{
    workload: WorkloadItem;
    repo: string;
    containerName: string;
    currentTag: string;
  } | null>(null);
  const [targetTag, setTargetTag] = useState('');
  const [actionBusy, setActionBusy] = useState(false);
  const [actionErr, setActionErr] = useState('');

  const loadAll = async (fresh = false) => {
    setLoading(true);
    try {
      const [w, s] = await Promise.all([api.workloads(fresh), api.sources()]);
      setInv(w);
      setSources(s);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void loadAll(false);
  }, []);

  useEffect(() => {
    const closeMenus = () => setOpenMenuKey(null);
    window.addEventListener('click', closeMenus);
    return () => window.removeEventListener('click', closeMenus);
  }, []);

  const sourceByRepo = useMemo(() => {
    const m = new Map<string, SourceItem>();
    for (const s of sources) {
      m.set(s.imageRef, s);
    }
    return m;
  }, [sources]);

  const groups = useMemo<GroupedWorkloads[]>(() => {
    if (!inv) return [];
    const q = query.trim().toLowerCase();
    const byRepo = new Map<string, GroupedWorkloads>();

    for (const w of inv.workloads) {
      if (clusterFilter !== 'all' && w.cluster !== clusterFilter) continue;
      for (const c of w.containers) {
        if (
          q &&
          !w.name.toLowerCase().includes(q) &&
          !w.namespace.toLowerCase().includes(q) &&
          !c.repo.toLowerCase().includes(q) &&
          !c.tag.toLowerCase().includes(q)
        ) {
          continue;
        }
        let grp = byRepo.get(c.repo);
        if (!grp) {
          grp = {
            repo: c.repo,
            source: sourceByRepo.get(c.repo),
            tags: [],
            rows: [],
          };
          byRepo.set(c.repo, grp);
        }
        if (!grp.tags.includes(c.tag)) {
          grp.tags.push(c.tag);
        }
        grp.rows.push({
          workload: w,
          containerName: c.name,
          tag: c.tag,
          init: c.init,
        });
      }
    }

    return Array.from(byRepo.values()).sort((a, b) =>
      shortImageName(a.repo).localeCompare(shortImageName(b.repo)),
    );
  }, [inv, clusterFilter, query, sourceByRepo]);

  const handleTogglePause = async (w: WorkloadItem) => {
    setActionErr('');
    try {
      await api.pause({
        cluster: w.cluster,
        namespace: w.namespace,
        kind: w.kind,
        name: w.name,
        paused: !w.paused,
      });
      await loadAll(true);
    } catch (err) {
      setActionErr(err instanceof Error ? err.message : String(err));
    }
  };

  const handleRolloutSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!rolloutModal || !targetTag.trim()) return;
    setActionBusy(true);
    setActionErr('');
    try {
      await api.rollout({
        cluster: rolloutModal.workload.cluster,
        namespace: rolloutModal.workload.namespace,
        kind: rolloutModal.workload.kind,
        name: rolloutModal.workload.name,
        image: rolloutModal.repo,
        container: rolloutModal.containerName,
        tag: targetTag.trim(),
      });
      setRolloutModal(null);
      setTargetTag('');
      await loadAll(true);
    } catch (err) {
      setActionErr(err instanceof Error ? err.message : String(err));
    } finally {
      setActionBusy(false);
    }
  };

  return (
    <div className="page-container page-wide">
      <header className="page-header">
        <div>
          <h1 className="serif-heading page-title">{t.servicesTitle}</h1>
          <p className="page-subtitle">{t.servicesSubtitle}</p>
        </div>
        <button
          type="button"
          className="btn btn-subtle"
          onClick={() => loadAll(true)}
          disabled={loading}
        >
          <RefreshCw size={15} className={loading ? 'spin' : ''} />
          <span>{t.refresh}</span>
        </button>
      </header>

      <div className="filter-bar">
        <div className="search-box">
          <Search size={15} className="text-faint" />
          <input
            type="text"
            value={query}
            onChange={(e) => {
              const v = e.target.value;
              setQuery(v);
              if (v) setSearchParams({ q: v }, { replace: true });
              else setSearchParams({}, { replace: true });
            }}
            placeholder={t.searchServicesPlaceholder}
          />
        </div>
        <div className="pill-group">
          <button
            type="button"
            className={`pill-btn ${clusterFilter === 'all' ? 'active' : ''}`}
            onClick={() => setClusterFilter('all')}
          >
            {t.allClusters}
          </button>
          {(inv?.clusters || []).map((cl) => (
            <button
              key={cl.name}
              type="button"
              className={`pill-btn mono ${clusterFilter === cl.name ? 'active' : ''}`}
              onClick={() => setClusterFilter(cl.name)}
            >
              {cl.name}
            </button>
          ))}
        </div>
      </div>

      {actionErr && <div className="inline-alert inline-alert-err">{actionErr}</div>}

      {groups.length === 0 ? (
        <div className="surface-card empty-state">{t.noServicesFound}</div>
      ) : (
        <div className="stack-md">
          {groups.map((grp) => (
            <section key={grp.repo} className="surface-card group-card">
              <div className="group-header">
                <div className="min-w-0">
                  <div className="row-gap-sm wrap">
                    <h2 className="group-title">{shortImageName(grp.repo)}</h2>
                    {grp.source && (
                      <Link to="/sources" className="source-link-pill">
                        <GitBranch size={12} strokeWidth={1.8} />
                        <span className="mono">
                          {grp.source.repo}@{grp.source.branch}
                        </span>
                      </Link>
                    )}
                    {grp.tags.length > 1 && (
                      <StatusBadge tone="warn">{t.tagDrift}</StatusBadge>
                    )}
                  </div>
                  <div className="mono text-xs text-muted truncate">{grp.repo}</div>
                </div>
                <div className="row-gap-xs wrap">
                  {grp.tags.map((tg) => (
                    <span key={tg} className="tag-chip mono">
                      {tg}
                    </span>
                  ))}
                </div>
              </div>

              <div className="service-rows">
                {grp.rows.map(({ workload: w, containerName, tag, init }) => {
                  const rowKey = `${grp.repo}:${w.cluster}:${w.namespace}:${w.kind}:${w.name}:${containerName}`;
                  return (
                    <div key={rowKey} className="service-row">
                      <div className="row-gap-sm min-w-0">
                        <span className="cluster-pill mono">{w.cluster}</span>
                        <span className="text-xs text-muted">{w.namespace}</span>
                        <span className="workload-ref">
                          <span className="text-faint text-xs">{w.kind}/</span>
                          {w.name}
                        </span>
                        {(containerName !== w.name || init) && (
                          <span className="text-xs text-faint mono">
                            [{init ? `init:${containerName}` : containerName}]
                          </span>
                        )}
                        {w.argoCD && (
                          <StatusBadge tone="warn" title={t.argoManagedHint}>
                            {t['res_argo-cd']}
                          </StatusBadge>
                        )}
                        {w.paused && (
                          <StatusBadge tone="neutral">{t.res_paused}</StatusBadge>
                        )}
                      </div>

                      <div className="row-gap-md shrink-0">
                        <span className="mono text-xs tag-inline">{tag}</span>

                        {w.kind === 'CronJob' ? (
                          <span className="text-xs text-muted mono row-gap-xs">
                            <Clock size={12} />
                            <span>{w.schedule || '—'}</span>
                            {w.suspend ? (
                              <StatusBadge tone="warn">{t.cronSuspended}</StatusBadge>
                            ) : (
                              w.lastScheduleTime && (
                                <span className="text-faint">
                                  · {formatRelativeTime(w.lastScheduleTime, locale)}
                                </span>
                              )
                            )}
                          </span>
                        ) : (
                          <span
                            className={`text-xs mono ${
                              w.healthy ? 'text-ok' : 'text-warn'
                            }`}
                          >
                            {w.ready}/{w.desired} {t.readyLabel}
                          </span>
                        )}

                        <div
                          className="menu-anchor"
                          onClick={(e) => e.stopPropagation()}
                        >
                          <button
                            type="button"
                            className="icon-btn"
                            disabled={w.argoCD}
                            title={w.argoCD ? t.argoManagedHint : undefined}
                            onClick={() =>
                              setOpenMenuKey(openMenuKey === rowKey ? null : rowKey)
                            }
                          >
                            <MoreHorizontal size={16} />
                          </button>

                          {openMenuKey === rowKey && !w.argoCD && (
                            <div className="dropdown-menu">
                              <button
                                type="button"
                                className="dropdown-item"
                                onClick={() => {
                                  setOpenMenuKey(null);
                                  setActionErr('');
                                  setTargetTag(tag);
                                  setRolloutModal({
                                    workload: w,
                                    repo: grp.repo,
                                    containerName,
                                    currentTag: tag,
                                  });
                                }}
                              >
                                <Tag size={14} />
                                <span>{t.deployTagAction}</span>
                              </button>
                              <button
                                type="button"
                                className="dropdown-item"
                                onClick={() => {
                                  setOpenMenuKey(null);
                                  void handleTogglePause(w);
                                }}
                              >
                                {w.paused ? <Play size={14} /> : <Pause size={14} />}
                                <span>
                                  {w.paused ? t.resumeAutoDeploy : t.pauseAutoDeploy}
                                </span>
                              </button>
                            </div>
                          )}
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            </section>
          ))}
        </div>
      )}

      <Modal
        open={rolloutModal !== null}
        title={t.rolloutModalTitle}
        onClose={() => setRolloutModal(null)}
        footer={
          <>
            <button
              type="button"
              className="btn btn-subtle"
              onClick={() => setRolloutModal(null)}
            >
              {t.cancel}
            </button>
            <button
              type="submit"
              form="rollout-form"
              className="btn btn-primary"
              disabled={!targetTag.trim() || actionBusy}
            >
              {actionBusy ? t.deploying : t.deployNowBtn}
            </button>
          </>
        }
      >
        {rolloutModal && (
          <form id="rollout-form" onSubmit={handleRolloutSubmit} className="stack-sm">
            <p className="text-sm text-muted">{t.rolloutModalDesc}</p>
            <div className="meta-box mono text-xs">
              <div>
                {rolloutModal.workload.cluster} · {rolloutModal.workload.namespace}/
                {rolloutModal.workload.kind}/{rolloutModal.workload.name}
              </div>
              <div className="text-muted">{rolloutModal.repo}</div>
            </div>
            <label className="field-label">
              <span>{t.newTagLabel}</span>
              <input
                type="text"
                className="text-input mono"
                value={targetTag}
                onChange={(e) => setTargetTag(e.target.value)}
                autoFocus
                required
              />
            </label>
          </form>
        )}
      </Modal>
    </div>
  );
}
