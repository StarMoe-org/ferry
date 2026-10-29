export interface MeResponse {
  sub: string;
  name: string;
  email: string;
  groups?: string[];
  authDisabled: boolean;
  publicURL: string;
  annotationPrefix: string;
  namespaces: string[];
  cnbConfigured: boolean;
  cnbRepo: string;
  cnbImagePrefix: string;
  githubTokenConfigured: boolean;
  githubWebhookConfigured: boolean;
  snapshotConfigured: boolean;
  snapshotRepo: string;
  snapshotBranch: string;
  snapshotDir: string;
  snapshotHourUTC: number;
}

export interface ClusterSummary {
  name: string;
  server: string;
  version?: string;
  reachable: boolean;
  error?: string;
  workloadCount: number;
  notReadyCount: number;
}

export interface ContainerItem {
  name: string;
  image: string;
  repo: string;
  tag: string;
  init?: boolean;
}

export interface WorkloadItem {
  cluster: string;
  namespace: string;
  kind: 'Deployment' | 'StatefulSet' | 'DaemonSet' | 'CronJob' | string;
  name: string;
  containers: ContainerItem[];
  desired: number;
  ready: number;
  updated: number;
  available: number;
  schedule?: string;
  suspend?: boolean;
  lastScheduleTime?: string;
  argoCD: boolean;
  paused: boolean;
  changeCause?: string;
  healthy: boolean;
}

export interface InventoryResponse {
  fetchedAt: string;
  clusters: ClusterSummary[];
  workloads: WorkloadItem[];
}

export type DeployResultKind =
  | 'deployed'
  | 'current'
  | 'paused'
  | 'argo-cd'
  | 'would-deploy'
  | 'error'
  | string;

export interface DeployResult {
  cluster: string;
  namespace: string;
  kind: string;
  name: string;
  container: string;
  from: string;
  to: string;
  result: DeployResultKind;
  error?: string;
}

export interface FerryEvent {
  id: string;
  at: string;
  kind: 'deploy' | 'rollout' | 'rollback' | 'build' | 'snapshot' | 'pause' | 'resume' | string;
  actor: string;
  image?: string;
  tag?: string;
  source?: string;
  sourceId?: string;
  commit?: string;
  logUrl?: string;
  status: 'ok' | 'partial' | 'error' | 'queued' | 'started' | 'skipped' | string;
  message?: string;
  results?: DeployResult[];
}

export interface SourceItem {
  id: string;
  repo: string;
  branch: string;
  builder: 'cnb' | 'external';
  image: string;
  imageRef: string;
  buildArgs?: string;
  createdAt: string;
}

export interface WebhookStatus {
  status: 'ok' | 'missing' | 'no-permission' | 'unconfigured' | string;
  webhookUrl?: string;
  hookId?: number;
  message?: string;
}

export interface TokenInfo {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  createdBy: string;
  lastUsed?: string;
}

export interface CreatedTokenResponse {
  token: string;
  info: TokenInfo;
}

export interface SnapshotResponse {
  status: 'committed' | 'unchanged' | 'queued' | string;
  commit?: string;
}
