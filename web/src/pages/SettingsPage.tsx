import { useEffect, useState } from 'react';
import { Camera, KeyRound, Plus, Trash2 } from 'lucide-react';
import { api } from '../api';
import { CopyBlock, StatusBadge } from '../components/UI';
import { formatRelativeTime, useI18n } from '../i18n';
import { buildCISnippet, buildGitHubActionsSnippet } from './SourcesPage';
import type {
  ClusterSummary,
  FerryEvent,
  MeResponse,
  TokenInfo,
} from '../types';

export function SettingsPage({ me }: { me: MeResponse }) {
  const { t, locale } = useI18n();

  const [tokens, setTokens] = useState<TokenInfo[]>([]);
  const [clusters, setClusters] = useState<ClusterSummary[]>([]);
  const [lastSnap, setLastSnap] = useState<FerryEvent | null>(null);

  const [newTokenName, setNewTokenName] = useState('');
  const [revealedToken, setRevealedToken] = useState<string | null>(null);
  const [creatingTok, setCreatingTok] = useState(false);

  const [snapBusy, setSnapBusy] = useState(false);
  const [snapNotice, setSnapNotice] = useState('');
  const [errMsg, setErrMsg] = useState('');

  const publicOrigin =
    me.publicURL || (typeof window !== 'undefined' ? window.location.origin : 'https://ferry.example.com');

  const loadAll = async () => {
    const [toks, cls, snapEvs] = await Promise.all([
      api.tokens(),
      api.clusters(false),
      api.events({ kind: 'snapshot', limit: 1 }),
    ]);
    setTokens(toks);
    setClusters(cls);
    setLastSnap(snapEvs[0] || null);
  };

  useEffect(() => {
    void loadAll();
  }, []);

  const handleCreateToken = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newTokenName.trim()) return;
    setCreatingTok(true);
    setErrMsg('');
    try {
      const created = await api.createToken(newTokenName.trim());
      setRevealedToken(created.token);
      setNewTokenName('');
      await loadAll();
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    } finally {
      setCreatingTok(false);
    }
  };

  const handleRevokeToken = async (id: string) => {
    setErrMsg('');
    try {
      await api.deleteToken(id);
      await loadAll();
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    }
  };

  const handleSnapshotNow = async () => {
    setSnapBusy(true);
    setSnapNotice('');
    setErrMsg('');
    try {
      const res = await api.snapshot();
      setSnapNotice(
        `${res.status}${res.commit ? ` (${res.commit.slice(0, 12)})` : ''}`,
      );
      await loadAll();
      if (res.status === 'queued') {
        window.setTimeout(() => void loadAll(), 600);
        window.setTimeout(() => void loadAll(), 2200);
      }
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err));
    } finally {
      setSnapBusy(false);
    }
  };

  return (
    <div className="page-container">
      <header className="page-header">
        <div>
          <h1 className="serif-heading page-title">{t.settingsTitle}</h1>
          <p className="page-subtitle">{t.settingsSubtitle}</p>
        </div>
      </header>

      {errMsg && <div className="inline-alert inline-alert-err">{errMsg}</div>}

      <div className="stack-lg">
        {/* 1. Deploy Tokens */}
        <section className="surface-card stack-sm">
          <div>
            <h2 className="section-title">{t.tokensSectionTitle}</h2>
            <p className="text-sm text-muted">{t.tokensSectionDesc}</p>
          </div>

          <form onSubmit={handleCreateToken} className="token-create-row">
            <input
              type="text"
              className="text-input"
              placeholder={t.tokenNamePlaceholder}
              value={newTokenName}
              onChange={(e) => setNewTokenName(e.target.value)}
            />
            <button
              type="submit"
              className="btn btn-primary shrink-0"
              disabled={!newTokenName.trim() || creatingTok}
            >
              <Plus size={15} />
              <span>{t.createTokenBtn}</span>
            </button>
          </form>

          {revealedToken && (
            <div className="token-reveal-box stack-xs">
              <div className="row-gap-xs text-xs text-warn">
                <KeyRound size={14} />
                <strong>{t.tokenCreatedWarning}</strong>
              </div>
              <CopyBlock code={revealedToken} />
            </div>
          )}

          {tokens.length === 0 ? (
            <p className="empty-inline">{t.noTokensYet}</p>
          ) : (
            <div className="data-table">
              {tokens.map((tok) => (
                <div key={tok.id} className="data-row">
                  <div className="row-gap-sm min-w-0">
                    <span className="font-medium">{tok.name}</span>
                    <span className="tag-chip mono">{tok.prefix}…</span>
                  </div>
                  <div className="row-gap-md text-xs text-muted shrink-0">
                    <span>{tok.createdBy}</span>
                    <span>
                      {t.tokenColLastUsed}:{' '}
                      {tok.lastUsed
                        ? formatRelativeTime(tok.lastUsed, locale)
                        : t.neverUsed}
                    </span>
                    <button
                      type="button"
                      className="btn btn-subtle btn-sm"
                      onClick={() => handleRevokeToken(tok.id)}
                    >
                      <Trash2 size={13} />
                      <span>{t.revokeBtn}</span>
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        {/* 2. Clusters */}
        <section className="surface-card stack-sm">
          <div>
            <h2 className="section-title">{t.clustersSectionTitle}</h2>
            <p className="text-sm text-muted">{t.clustersSectionDesc}</p>
          </div>
          <div className="data-table">
            {clusters.map((cl) => (
              <div key={cl.name} className="data-row">
                <div className="row-gap-sm">
                  <span
                    className={`status-dot ${cl.reachable ? 'dot-ok' : 'dot-err'}`}
                  />
                  <span className="mono font-medium">{cl.name}</span>
                  <span className="mono text-xs text-muted">{cl.server}</span>
                </div>
                <div className="row-gap-sm text-xs">
                  {cl.version && (
                    <span className="tag-chip mono">{cl.version}</span>
                  )}
                  <StatusBadge tone={cl.reachable ? 'ok' : 'err'}>
                    {cl.reachable ? t.clusterReachable : t.clusterUnreachable}
                  </StatusBadge>
                </div>
              </div>
            ))}
          </div>
        </section>

        {/* 3. Snapshots */}
        <section className="surface-card stack-sm">
          <div className="card-header">
            <div>
              <h2 className="section-title">{t.snapshotsSectionTitle}</h2>
              <p className="text-sm text-muted">{t.snapshotsSectionDesc}</p>
            </div>
            {me.snapshotConfigured && (
              <button
                type="button"
                className="btn btn-subtle shrink-0"
                disabled={snapBusy}
                onClick={handleSnapshotNow}
              >
                <Camera size={15} />
                <span>{snapBusy ? t.snapshottingBtn : t.snapshotNowBtn}</span>
              </button>
            )}
          </div>

          {snapNotice && (
            <div className="inline-alert inline-alert-ok mono">{snapNotice}</div>
          )}

          {!me.snapshotConfigured ? (
            <p className="empty-inline">{t.snapshotNotConfigured}</p>
          ) : (
            <div className="meta-box text-xs stack-xs">
              <div>
                <span className="text-muted">Git: </span>
                <span className="mono">
                  {me.snapshotRepo} ({me.snapshotBranch}) → /{me.snapshotDir}
                </span>
              </div>
              <div>
                <span className="text-muted">{t.snapshotScheduleLabel}: </span>
                <span className="mono">
                  {String(me.snapshotHourUTC).padStart(2, '0')}:00 UTC
                </span>
              </div>
              {lastSnap && (
                <div>
                  <span className="text-muted">{t.snapshotLastEvent}: </span>
                  <span className="mono">
                    {lastSnap.message || lastSnap.status}
                    {lastSnap.commit ? ` · ${lastSnap.commit.slice(0, 12)}` : ''}
                  </span>{' '}
                  <span className="text-faint">
                    ({formatRelativeTime(lastSnap.at, locale)})
                  </span>
                </div>
              )}
            </div>
          )}
        </section>

        {/* 4. Connect CI */}
        <section className="surface-card stack-sm">
          <div>
            <h2 className="section-title">{t.connectCISectionTitle}</h2>
            <p className="text-sm text-muted">{t.connectCISectionDesc}</p>
          </div>
          <CopyBlock
            label="curl"
            code={buildCISnippet(publicOrigin, 'ghcr.io/org/service', 'org/service')}
          />
          <CopyBlock
            label="GitHub Actions"
            code={buildGitHubActionsSnippet(publicOrigin, 'ghcr.io/org/service')}
          />
        </section>
      </div>
    </div>
  );
}
