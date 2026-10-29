import type {
  ClusterSummary,
  CreatedTokenResponse,
  DeployResult,
  FerryEvent,
  InventoryResponse,
  MeResponse,
  SnapshotResponse,
  SourceItem,
  TokenInfo,
  WebhookStatus,
} from './types';

export class APIError extends Error {
  status: number;
  data?: unknown;

  constructor(status: number, message: string, data?: unknown) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method || 'GET').toUpperCase();
  const headers = new Headers(init.headers || {});
  if (method !== 'GET' && method !== 'HEAD') {
    headers.set('Content-Type', 'application/json');
  }
  headers.set('Accept', 'application/json');

  const resp = await fetch(path, {
    ...init,
    method,
    headers,
    credentials: 'same-origin',
  });

  if (resp.status === 204) {
    return undefined as T;
  }

  const text = await resp.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = text;
    }
  }

  if (!resp.ok) {
    const errMsg =
      typeof parsed === 'object' && parsed !== null && 'error' in parsed
        ? String((parsed as { error: unknown }).error)
        : text || `HTTP ${resp.status}`;
    throw new APIError(resp.status, errMsg, parsed);
  }

  return parsed as T;
}

export const api = {
  me: () => request<MeResponse>('/api/me'),

  logout: () => request<void>('/auth/logout', { method: 'POST', body: '{}' }),

  clusters: (fresh = false) =>
    request<ClusterSummary[]>(`/api/clusters${fresh ? '?fresh=1' : ''}`),

  workloads: (fresh = false) =>
    request<InventoryResponse>(`/api/workloads${fresh ? '?fresh=1' : ''}`),

  previewDeploy: (image: string, tag: string, source = '') =>
    request<DeployResult[]>('/api/deploy/preview', {
      method: 'POST',
      body: JSON.stringify({ image, tag, source }),
    }),

  deploy: async (image: string, tag: string, source = ''): Promise<DeployResult[]> => {
    try {
      return await request<DeployResult[]>('/api/deploy', {
        method: 'POST',
        body: JSON.stringify({ image, tag, source }),
      });
    } catch (err) {
      // POST /api/deploy returns 500 with the Results array if any workload failed to patch.
      if (err instanceof APIError && Array.isArray(err.data)) {
        return err.data as DeployResult[];
      }
      throw err;
    }
  },

  rollout: (payload: {
    cluster: string;
    namespace: string;
    kind: string;
    name: string;
    image: string;
    container?: string;
    tag: string;
  }) =>
    request<FerryEvent>('/api/rollout', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  rollback: (eventId: string) =>
    request<FerryEvent>('/api/rollback', {
      method: 'POST',
      body: JSON.stringify({ eventId }),
    }),

  pause: (payload: {
    cluster: string;
    namespace: string;
    kind: string;
    name: string;
    paused: boolean;
  }) =>
    request<FerryEvent>('/api/pause', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  sources: () => request<SourceItem[]>('/api/sources'),

  createSource: (payload: {
    repo: string;
    branch: string;
    builder: 'cnb' | 'external';
    image: string;
    buildArgs?: string;
  }) =>
    request<SourceItem>('/api/sources', {
      method: 'POST',
      body: JSON.stringify(payload),
    }),

  updateSource: (
    id: string,
    payload: {
      repo: string;
      branch: string;
      builder: 'cnb' | 'external';
      image: string;
      buildArgs?: string;
    },
  ) =>
    request<SourceItem>(`/api/sources/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(payload),
    }),

  deleteSource: (id: string) =>
    request<void>(`/api/sources/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      body: '{}',
    }),

  buildSource: (id: string) =>
    request<FerryEvent>(`/api/sources/${encodeURIComponent(id)}/build`, {
      method: 'POST',
      body: '{}',
    }),

  webhookStatus: (id: string) =>
    request<WebhookStatus>(`/api/sources/${encodeURIComponent(id)}/webhook`),

  setupWebhook: (id: string) =>
    request<WebhookStatus>(`/api/sources/${encodeURIComponent(id)}/webhook`, {
      method: 'POST',
      body: '{}',
    }),

  events: (params?: { kind?: string; image?: string; limit?: number }) => {
    const q = new URLSearchParams();
    if (params?.kind) q.set('kind', params.kind);
    if (params?.image) q.set('image', params.image);
    if (params?.limit) q.set('limit', String(params.limit));
    const qs = q.toString();
    return request<FerryEvent[]>(`/api/events${qs ? `?${qs}` : ''}`);
  },

  tokens: () => request<TokenInfo[]>('/api/tokens'),

  createToken: (name: string) =>
    request<CreatedTokenResponse>('/api/tokens', {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),

  deleteToken: (id: string) =>
    request<void>(`/api/tokens/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      body: '{}',
    }),

  snapshot: () =>
    request<SnapshotResponse>('/api/snapshot', {
      method: 'POST',
      body: '{}',
    }),
};

export function shortImageName(repo: string): string {
  const parts = repo.split('/');
  return parts.length > 1 ? parts[parts.length - 1] : repo;
}

export function parseComposerInput(raw: string): { image: string; tag: string } | null {
  const trimmed = raw.trim();
  if (!trimmed || /\s/.test(trimmed)) return null;
  const lastSlash = trimmed.lastIndexOf('/');
  const lastColon = trimmed.lastIndexOf(':');
  if (lastColon <= lastSlash || lastColon === trimmed.length - 1) {
    return null;
  }
  return {
    image: trimmed.slice(0, lastColon),
    tag: trimmed.slice(lastColon + 1),
  };
}
