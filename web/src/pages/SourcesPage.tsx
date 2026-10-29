import { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router';
import {
  Code2,
  ExternalLink,
  GitBranch,
  Layers,
  Pencil,
  Play,
  Plus,
  Trash2,
  Webhook,
} from 'lucide-react';
import { api } from '../api';
import { CopyBlock, eventStatusTone, Modal, StatusBadge } from '../components/UI';
import { formatRelativeTime, useI18n } from '../i18n';
import { labelForStatus } from './HomePage';
import type {
  FerryEvent,
  InventoryResponse,
  MeResponse,
  SourceItem,
  WebhookStatus,
} from '../types';

export function SourcesPage({ me }: { me: MeResponse }) {
  const { t, locale } = useI18n();

  const [sources, setSources] = useState<SourceItem[]>([]);
  const [inv, setInv] = useState<InventoryResponse | null>(null);
  const [buildEvents, setBuildEvents] = useState<FerryEvent[]>([]);
  const [hooks, setHooks] = useState<Record<string, WebhookStatus | 'loading'>>({});
  const [notice, setNotice] = useState('');
  const [errMsg, setErrMsg] = useState('');

  // Modals
  const [editing, setEditing] = useState<SourceItem | 'new' | null>(null);
  const [snippetSource, setSnippetSource] = useState<SourceItem | null>(null);
  const [manualHookSource, setManualHookSource] = useState<{
    source: SourceItem;
    url: string;
  } | null>(null);
  const [deletingSource, setDeletingSource] = useState<SourceItem | null>(null);

  // Form state
  const [repo, setRepo] = useState('');
  const [branch, setBranch] = useState('main');
  const [builder, setBuilder] = useState<'cnb' | 'external'>('cnb');
  const [image, setImage] = useState('');
  const [buildArgs, setBuildArgs] = useState('');
  const [saving, setSaving] = useState(false);

  const publicOrigin =
    me.publicURL || (typeof window !== 'undefined' ? window.location.origin : 'https://ferry.example.com');

  const loadAll = async () => {
    const [srcs, w, evs] = await Promise.all([
      api.sources(),
      api.workloads(false),
      api.events({ kind: 'build', limit: 100 }),
    ]);
    setSources(srcs);
    setInv(w);
    setBuildEvents(evs);

    // Lazy fetch webhook status per source without blocking render!
    for (const s of srcs) {
      if (s.builder !== 'cnb') continue;
      setHooks((prev) => ({ ...prev, [s.id]: 'loading' }));
      api
        .webhookStatus(s.id)
        .then((st) => setHooks((prev) => ({ ...prev, [s.id]: st })))
        .catch(() =>
          setHooks((prev) => ({
            ...prev,
            [s.id]: { status: 'no-permission', webhookUrl: `${publicOrigin}/webhook/github` },
          })),
        );
    }
  };

  useEffect(() => {
    void loadAll();
  }, []);

  const serviceCountByRepo = useMemo(() => {
    const m = new Map<string, number>();
    for (const w of inv?.workloads || []) {
      const reposInWorkload = new Set(w.containers.map((c) => c.repo));
      for (const r of reposInWorkload) {
        m.set(r, (m.get(r) || 0) + 1);
      }
    }
    return m;
  }, [inv]);

  const lastBuildBySource = useMemo(() => {
    const m = new Map<string, FerryEvent>();
    for (const ev of buildEvents) {
      if (ev.sourceId && !m.has(ev.sourceId)) {
        m.set(ev.sourceId, ev);
      } else if (ev.image && !m.has(ev.image)) {
        m.set(ev.image, ev);
      }
    }
    return m;
  }, [buildEvents]);

  const openEditor = (target: SourceItem | 'new') => {
    setErrMsg('');
    if (target === 'new') {
      setRepo('');
      setBranch('main');
      setBuilder(me.cnbConfigured ? 'cnb' : 'external');
      setImage('');
      setBuildArgs('');
    } else {
      setRepo(target.repo);
      setBranch(target.branch);
      setBuilder(target.builder);
      setImage(target.image);
      setBuildArgs(target.buildArgs || '');
    }
    setEditing(target);
  };

  const previewImageRef = useMemo(() => {
    const trimmed = image.trim() || repo.split('/')[1] || 'app';
    if (builder === 'cnb' && me.cnbImagePrefix && !trimmed.includes('/')) {
      return `${me.cnbImagePrefix.replace(/\/$/, '')}/${trimmed}`;
    }
    return trimmed;
  }, [builder, image, repo, me.cnbImagePrefix]);

  const handleSaveSource = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setErrMsg('');
    try {
      const finalImg = image.trim() || repo.split('/')[1] || '';
      const payload = {
        repo: repo.trim(),
        branch: branch.trim() || 'main',
        builder,
        image: finalImg,
        buildArgs: builder === 'cnb' ? buildArgs.trim() : '',
      };
      if (editing === 'new') {
        await api.createSource(payload);
      } else if (editing) {
        await api.updateSource(editing.id, payload);
      }
      setEditing(null);
      await loadAll();
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleBuildNow = async (src: SourceItem) => {
    setErrMsg('');
    setNotice('');
    try {
      const ev = await api.buildSource(src.id);
      setNotice(`${t.buildingToast}: ${src.repo}@${ev.tag || src.branch}`);
      const evs = await api.events({ kind: 'build', limit: 100 });
      setBuildEvents(evs);
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    }
  };

  const handleSetupWebhook = async (src: SourceItem) => {
    setErrMsg('');
    try {
      const st = await api.setupWebhook(src.id);
      setHooks((prev) => ({ ...prev, [src.id]: st }));
      if (st.status === 'no-permission') {
        setManualHookSource({
          source: src,
          url: st.webhookUrl || `${publicOrigin}/webhook/github`,
        });
      }
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    }
  };

  const handleConfirmDelete = async () => {
    if (!deletingSource) return;
    try {
      await api.deleteSource(deletingSource.id);
      setDeletingSource(null);
      await loadAll();
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="page-container">
      <header className="page-header">
        <div>
          <h1 className="serif-heading page-title">{t.sourcesTitle}</h1>
          <p className="page-subtitle">{t.sourcesSubtitle}</p>
        </div>
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => openEditor('new')}
        >
          <Plus size={16} />
          <span>{t.newSourceBtn}</span>
        </button>
      </header>

      {notice && <div className="inline-alert inline-alert-ok">{notice}</div>}
      {errMsg && <div className="inline-alert inline-alert-err">{errMsg}</div>}

      {sources.length === 0 ? (
        <div className="surface-card empty-state">{t.noSourcesYet}</div>
      ) : (
        <div className="stack-md">
          {sources.map((src) => {
            const svcCount = serviceCountByRepo.get(src.imageRef) || 0;
            const lastBuild =
              lastBuildBySource.get(src.id) || lastBuildBySource.get(src.imageRef);
            const hook = hooks[src.id];

            return (
              <section key={src.id} className="surface-card source-card">
                <div className="source-top">
                  <div className="min-w-0">
                    <div className="row-gap-sm wrap">
                      <GitBranch size={16} className="text-accent shrink-0" />
                      <span className="source-repo">{src.repo}</span>
                      <span className="tag-chip mono">@{src.branch}</span>
                      <StatusBadge tone={src.builder === 'cnb' ? 'accent' : 'neutral'}>
                        {src.builder === 'cnb' ? t.builderCNB : t.builderExternal}
                      </StatusBadge>
                    </div>
                    <div className="mono text-xs text-muted truncate mt-xs">
                      {src.imageRef}
                    </div>
                  </div>

                  <div className="row-gap-xs shrink-0">
                    {src.builder === 'cnb' && me.cnbConfigured && (
                      <button
                        type="button"
                        className="btn btn-subtle btn-sm"
                        onClick={() => handleBuildNow(src)}
                      >
                        <Play size={13} />
                        <span>{t.buildNowBtn}</span>
                      </button>
                    )}
                    <button
                      type="button"
                      className="btn btn-subtle btn-sm"
                      onClick={() => setSnippetSource(src)}
                    >
                      <Code2 size={14} />
                      <span>{t.ciSnippetBtn}</span>
                    </button>
                    <button
                      type="button"
                      className="icon-btn"
                      onClick={() => openEditor(src)}
                      title={t.edit}
                    >
                      <Pencil size={15} />
                    </button>
                    <button
                      type="button"
                      className="icon-btn"
                      onClick={() => setDeletingSource(src)}
                      title={t.delete}
                    >
                      <Trash2 size={15} />
                    </button>
                  </div>
                </div>

                <div className="source-meta-bar">
                  <Link
                    to={`/services?q=${encodeURIComponent(src.imageRef)}`}
                    className="subtle-link row-gap-xs text-xs"
                  >
                    <Layers size={13} />
                    <span>
                      {svcCount} {t.workloadsCount}
                    </span>
                  </Link>

                  {src.builder === 'cnb' && (
                    <div className="row-gap-xs text-xs">
                      <Webhook size={13} className="text-muted" />
                      {hook === 'loading' || !hook ? (
                        <span className="text-faint">{t.webhookChecking}</span>
                      ) : hook.status === 'ok' ? (
                        <StatusBadge tone="ok">{t.webhookOk}</StatusBadge>
                      ) : hook.status === 'missing' ? (
                        <button
                          type="button"
                          className="link-btn text-accent"
                          onClick={() => handleSetupWebhook(src)}
                        >
                          {t.setupWebhookBtn}
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="link-btn text-muted"
                          onClick={() =>
                            setManualHookSource({
                              source: src,
                              url: hook.webhookUrl || `${publicOrigin}/webhook/github`,
                            })
                          }
                        >
                          {t.webhookNoPerm}
                        </button>
                      )}
                    </div>
                  )}

                  <div className="row-gap-xs text-xs text-muted ml-auto">
                    <span>{t.lastBuildLabel}:</span>
                    {lastBuild ? (
                      <>
                        <StatusBadge tone={eventStatusTone(lastBuild.status)}>
                          {labelForStatus(lastBuild.status, t)}
                        </StatusBadge>
                        {lastBuild.tag && (
                          <span className="mono">{lastBuild.tag}</span>
                        )}
                        <span className="text-faint">
                          {formatRelativeTime(lastBuild.at, locale)}
                        </span>
                        {lastBuild.logUrl && (
                          <a
                            href={lastBuild.logUrl}
                            target="_blank"
                            rel="noreferrer"
                            className="subtle-link row-gap-xs"
                          >
                            <span>{t.buildLogsLink}</span>
                            <ExternalLink size={12} />
                          </a>
                        )}
                      </>
                    ) : (
                      <span className="text-faint">{t.noBuildsYet}</span>
                    )}
                  </div>
                </div>
              </section>
            );
          })}
        </div>
      )}

      {/* New / Edit Source Modal */}
      <Modal
        open={editing !== null}
        title={editing === 'new' ? t.newSourceTitle : t.editSourceTitle}
        onClose={() => setEditing(null)}
        width={560}
        footer={
          <>
            <button
              type="button"
              className="btn btn-subtle"
              onClick={() => setEditing(null)}
            >
              {t.cancel}
            </button>
            <button
              type="submit"
              form="source-form"
              className="btn btn-primary"
              disabled={saving || !repo.trim()}
            >
              {t.save}
            </button>
          </>
        }
      >
        <form id="source-form" onSubmit={handleSaveSource} className="stack-sm">
          <div className="grid-2">
            <label className="field-label">
              <span>{t.repoLabel}</span>
              <input
                type="text"
                className="text-input mono"
                placeholder="acme/web-portal"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                required
              />
            </label>
            <label className="field-label">
              <span>{t.branchLabel}</span>
              <input
                type="text"
                className="text-input mono"
                placeholder="main"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
                required
              />
            </label>
          </div>

          <div className="field-label">
            <span>{t.builderLabel}</span>
            <div className="pill-group">
              <button
                type="button"
                className={`pill-btn ${builder === 'cnb' ? 'active' : ''}`}
                onClick={() => setBuilder('cnb')}
              >
                {t.builderCNB}
              </button>
              <button
                type="button"
                className={`pill-btn ${builder === 'external' ? 'active' : ''}`}
                onClick={() => setBuilder('external')}
              >
                {t.builderExternal}
              </button>
            </div>
          </div>

          <label className="field-label">
            <span>
              {builder === 'cnb' ? t.imageLabelCNB : t.imageLabelExternal}
            </span>
            <input
              type="text"
              className="text-input mono"
              placeholder={
                builder === 'cnb' ? 'web-portal' : 'ghcr.io/acme/web-portal'
              }
              value={image}
              onChange={(e) => setImage(e.target.value)}
              required={builder === 'external'}
            />
          </label>

          {builder === 'cnb' && (
            <label className="field-label">
              <span>{t.buildArgsLabel}</span>
              <input
                type="text"
                className="text-input mono"
                placeholder="VITE_ENV=production"
                value={buildArgs}
                onChange={(e) => setBuildArgs(e.target.value)}
              />
            </label>
          )}

          <div className="meta-box text-xs">
            <span className="text-muted">{t.resolvedImageLabel}: </span>
            <span className="mono">{previewImageRef}</span>
          </div>

          {builder === 'external' && (
            <CopyBlock
              label="CI Deploy Call"
              code={buildCISnippet(publicOrigin, previewImageRef, repo || 'owner/repo')}
            />
          )}
        </form>
      </Modal>

      {/* CI Snippet Modal */}
      <Modal
        open={snippetSource !== null}
        title={t.ciSnippetBtn}
        onClose={() => setSnippetSource(null)}
        width={600}
        footer={
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => setSnippetSource(null)}
          >
            {t.close}
          </button>
        }
      >
        {snippetSource && (
          <div className="stack-sm">
            <CopyBlock
              label="curl"
              code={buildCISnippet(
                publicOrigin,
                snippetSource.imageRef,
                snippetSource.repo,
              )}
            />
            <CopyBlock
              label="GitHub Actions"
              code={buildGitHubActionsSnippet(publicOrigin, snippetSource.imageRef)}
            />
          </div>
        )}
      </Modal>

      {/* Manual Webhook Instructions Modal */}
      <Modal
        open={manualHookSource !== null}
        title={t.manualWebhookTitle}
        onClose={() => setManualHookSource(null)}
        footer={
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => setManualHookSource(null)}
          >
            {t.close}
          </button>
        }
      >
        {manualHookSource && (
          <div className="stack-sm">
            <p className="text-sm text-muted">{t.manualWebhookDesc}</p>
            <CopyBlock label={t.payloadUrlLabel} code={manualHookSource.url} />
            <div className="meta-box text-xs stack-xs">
              <div>
                <strong>{t.contentTypeLabel}:</strong>{' '}
                <span className="mono">application/json</span>
              </div>
              <div>
                <strong>{t.eventsLabel}:</strong>{' '}
                <span className="mono">Just the push event</span>
              </div>
              <div className="text-muted">{t.secretNote}</div>
            </div>
          </div>
        )}
      </Modal>

      {/* Delete Confirmation Modal */}
      <Modal
        open={deletingSource !== null}
        title={t.delete}
        onClose={() => setDeletingSource(null)}
        footer={
          <>
            <button
              type="button"
              className="btn btn-subtle"
              onClick={() => setDeletingSource(null)}
            >
              {t.cancel}
            </button>
            <button
              type="button"
              className="btn btn-danger"
              onClick={handleConfirmDelete}
            >
              {t.delete}
            </button>
          </>
        }
      >
        <p className="text-sm">{t.deleteSourceConfirm}</p>
      </Modal>
    </div>
  );
}

export function buildCISnippet(origin: string, imageRef: string, repo: string): string {
  return `curl -fsS -X POST "${origin.replace(/\/$/, '')}/deploy" \\
  -H "Authorization: Bearer \${FERRY_TOKEN}" \\
  -H "Content-Type: application/json" \\
  -d '{"image":"${imageRef}","tag":"'"\${GIT_SHA:0:12}"'","source":"github:${repo}@'"\${GIT_SHA:0:12}"'"}'`;
}

export function buildGitHubActionsSnippet(origin: string, imageRef: string): string {
  return `- name: Roll out via Ferry
  env:
    FERRY_TOKEN: \${{ secrets.FERRY_TOKEN }}
    SHORT_SHA: \${{ github.sha }}
  run: |
    TAG="\${SHORT_SHA:0:12}"
    curl -fsS -X POST "${origin.replace(/\/$/, '')}/deploy" \\
      -H "Authorization: Bearer \${FERRY_TOKEN}" \\
      -H "Content-Type: application/json" \\
      -d "{\\"image\\":\\"${imageRef}\\",\\"tag\\":\\"\${TAG}\\",\\"source\\":\\"github:\${{ github.repository }}@\${TAG}\\"}"`;
}
