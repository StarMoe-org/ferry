import { createContext, useContext, useState, type ReactNode } from 'react';

export type Locale = 'en' | 'zh-CN';

const STORAGE_KEY = 'ferry.locale';

const en = {
  // Navigation & shell
  navHome: 'Home',
  navServices: 'Services',
  navSources: 'Sources',
  navActivity: 'Activity',
  navSettings: 'Settings',
  newDeploy: 'New deploy',
  collapseSidebar: 'Collapse sidebar',
  expandSidebar: 'Expand sidebar',
  themeLabel: 'Appearance',
  themeLight: 'Light',
  themeDark: 'Dark',
  themeSystem: 'System',
  languageLabel: 'Language',
  signOut: 'Sign out',
  demoBadge: 'Demo',
  signInTitle: 'Sign in to Ferry',
  signInSubtitle: 'Multi-cluster Kubernetes image rollouts, builds, and GitOps snapshots.',
  signInButton: 'Continue with Single Sign-On',

  // Greetings
  greetMorning: 'Good morning',
  greetAfternoon: 'Good afternoon',
  greetEvening: 'Good evening',
  homeSubtitle: 'Roll out a container image across your Kubernetes clusters.',

  // Composer & dry-run preview
  composerPlaceholder: 'Paste image:tag, e.g. ghcr.io/acme/web:v1.4.2',
  composerHint: 'Press / to focus · Enter to preview dry-run',
  composerSourcePlaceholder: 'Optional source note (e.g. github:acme/web@a81f29c)',
  previewBtn: 'Preview',
  previewing: 'Checking…',
  deployNowBtn: 'Deploy',
  deploying: 'Deploying…',
  cancel: 'Cancel',
  close: 'Close',
  save: 'Save',
  delete: 'Delete',
  edit: 'Edit',
  refresh: 'Refresh',
  copy: 'Copy',
  copied: 'Copied',
  recentImages: 'Recent images',
  previewTitle: 'Dry-run preview',
  deployCompleteTitle: 'Deployment result',
  noMatchingWorkloads: 'No workloads matched this image repository across configured namespaces.',
  viewInActivity: 'View in Activity',
  composerInvalid: 'Enter an image and tag in image:tag format.',

  // Home cards
  clustersCardTitle: 'Clusters',
  recentActivityTitle: 'Recent activity',
  viewAll: 'View all',
  clusterReachable: 'Healthy',
  clusterUnreachable: 'Unreachable',
  workloadsCount: 'workloads',
  notReadyCount: 'not ready',
  noActivityYet: 'No deployment or build events recorded yet.',

  // Result & Event statuses
  res_deployed: 'Deployed',
  res_current: 'Up to date',
  'res_would-deploy': 'Will update',
  res_paused: 'Paused',
  'res_argo-cd': 'Argo CD',
  res_error: 'Error',

  st_ok: 'Succeeded',
  st_partial: 'Partial',
  st_error: 'Failed',
  st_queued: 'Queued',
  st_started: 'Building',
  st_skipped: 'Skipped',

  kind_deploy: 'Deploy',
  kind_rollout: 'Rollout',
  kind_rollback: 'Rollback',
  kind_build: 'Build',
  kind_snapshot: 'Snapshot',
  kind_pause: 'Pause',

  // Services page
  servicesTitle: 'Services',
  servicesSubtitle: 'Live workloads across clusters, grouped by container image repository.',
  searchServicesPlaceholder: 'Filter by workload, namespace, or image…',
  allClusters: 'All clusters',
  tagsInUse: 'Tags in use',
  tagDrift: 'Tag drift',
  readyLabel: 'ready',
  cronSuspended: 'Suspended',
  lastRun: 'last',
  deployTagAction: 'Deploy tag…',
  pauseAutoDeploy: 'Pause auto-deploy',
  resumeAutoDeploy: 'Resume auto-deploy',
  argoManagedHint: 'Managed by Argo CD — skipped automatically',
  rolloutModalTitle: 'Roll out image tag',
  rolloutModalDesc: 'Updates only this workload, even if auto-deploy is paused.',
  newTagLabel: 'Target tag',
  noServicesFound: 'No workloads match the current filter.',

  // Sources page
  sourcesTitle: 'Sources',
  sourcesSubtitle: 'GitHub repositories connected to CNB builds or external CI pipelines.',
  newSourceBtn: 'New source',
  editSourceTitle: 'Edit source',
  newSourceTitle: 'Connect repository source',
  repoLabel: 'GitHub repository',
  branchLabel: 'Branch',
  builderLabel: 'Builder',
  builderCNB: 'CNB build',
  builderExternal: 'External CI',
  imageLabelCNB: 'Image name',
  imageLabelExternal: 'Full image repository',
  buildArgsLabel: 'Build arguments (optional)',
  resolvedImageLabel: 'Target image',
  buildNowBtn: 'Build now',
  buildingToast: 'Build queued',
  ciSnippetBtn: 'CI snippet',
  webhookOk: 'Webhook active',
  webhookMissing: 'Webhook missing',
  webhookNoPerm: 'Manual webhook',
  webhookChecking: 'Checking hook…',
  setupWebhookBtn: 'Set up webhook',
  manualWebhookTitle: 'Configure GitHub webhook',
  manualWebhookDesc:
    'Ferry does not have admin permission on this repository. Add a webhook in your GitHub repository Settings → Webhooks:',
  payloadUrlLabel: 'Payload URL',
  contentTypeLabel: 'Content type',
  eventsLabel: 'Trigger events',
  secretNote: 'Use the webhookSecret configured in ferry.yaml.',
  lastBuildLabel: 'Last build',
  buildLogsLink: 'Build logs',
  noBuildsYet: 'No builds yet',
  noSourcesYet: 'No sources registered yet. Connect a GitHub repository to track builds and services.',
  deleteSourceConfirm: 'Remove this source? Existing Kubernetes workloads will not be affected.',

  // Activity page
  activityTitle: 'Activity',
  activitySubtitle: 'Chronological audit log of deployments, builds, rollbacks, and snapshots.',
  allKinds: 'All events',
  filterByImagePlaceholder: 'Filter by image repository…',
  rollbackBtn: 'Roll back',
  rollbackModalTitle: 'Confirm rollback',
  rollbackModalDesc:
    'Ferry will revert each workload below only if its live container image still matches the tag set by this event.',
  confirmRollbackBtn: 'Roll back workloads',
  detailsShow: 'Show workloads',
  detailsHide: 'Hide workloads',

  // Settings page
  settingsTitle: 'Settings',
  settingsSubtitle: 'Deploy tokens, cluster connections, GitOps snapshots, and CI integration.',
  tokensSectionTitle: 'Deploy tokens',
  tokensSectionDesc:
    'Scoped bearer tokens for GitHub Actions or external CI pipelines calling POST /deploy.',
  tokenNamePlaceholder: 'Token name (e.g. github-actions-prod)',
  createTokenBtn: 'Create token',
  tokenCreatedWarning: 'Copy this token now — only its SHA-256 digest is stored and it cannot be shown again.',
  tokenColName: 'Name',
  tokenColPrefix: 'Prefix',
  tokenColCreatedBy: 'Created by',
  tokenColLastUsed: 'Last used',
  neverUsed: 'Never',
  revokeBtn: 'Revoke',
  noTokensYet: 'No deploy tokens created yet.',

  clustersSectionTitle: 'Clusters',
  clustersSectionDesc: 'Configured in ferry.yaml or clustersFile (read-only).',

  snapshotsSectionTitle: 'Cluster snapshots',
  snapshotsSectionDesc: 'Cleaned Kubernetes manifests and SOPS-encrypted secrets committed to Git.',
  snapshotNowBtn: 'Snapshot now',
  snapshottingBtn: 'Snapshotting…',
  snapshotNotConfigured: 'Snapshots are not configured on this Ferry instance.',
  snapshotScheduleLabel: 'Daily schedule',
  snapshotLastEvent: 'Latest snapshot',

  connectCISectionTitle: 'Connect CI',
  connectCISectionDesc: 'Trigger multi-cluster image rollouts from any CI runner with a single HTTP call.',
};

export type Messages = typeof en;

const zhCN: Messages = {
  navHome: '首页',
  navServices: '服务',
  navSources: '代码源',
  navActivity: '动态',
  navSettings: '设置',
  newDeploy: '发布镜像',
  collapseSidebar: '收起侧栏',
  expandSidebar: '展开侧栏',
  themeLabel: '外观主题',
  themeLight: '浅色',
  themeDark: '深色',
  themeSystem: '跟随系统',
  languageLabel: '界面语言',
  signOut: '退出登录',
  demoBadge: '演示模式',
  signInTitle: '登录 Ferry',
  signInSubtitle: '多集群 Kubernetes 镜像发布、构建联动与 GitOps 快照中心。',
  signInButton: '使用单点登录 (SSO) 继续',

  greetMorning: '早上好',
  greetAfternoon: '下午好',
  greetEvening: '晚上好',
  homeSubtitle: '向所有受管 Kubernetes 集群安全推送容器镜像版本。',

  composerPlaceholder: '粘贴 镜像:标签，例如 ghcr.io/acme/web:v1.4.2',
  composerHint: '按 / 聚焦输入框 · 回车预览变更 (Dry-run)',
  composerSourcePlaceholder: '可选来源备注（如 github:acme/web@a81f29c）',
  previewBtn: '预览变更',
  previewing: '正在检查…',
  deployNowBtn: '确认发布',
  deploying: '正在发布…',
  cancel: '取消',
  close: '关闭',
  save: '保存',
  delete: '删除',
  edit: '编辑',
  refresh: '刷新',
  copy: '复制',
  copied: '已复制',
  recentImages: '最近发布镜像',
  previewTitle: '发布预检 (Dry-run)',
  deployCompleteTitle: '发布执行结果',
  noMatchingWorkloads: '受管命名空间中没有工作负载正在运行该镜像仓库。',
  viewInActivity: '在动态中查看',
  composerInvalid: '请输入 镜像仓库:标签 格式，例如 ghcr.io/org/app:v1.0.0。',

  clustersCardTitle: '集群状态',
  recentActivityTitle: '最近动态',
  viewAll: '查看全部',
  clusterReachable: '连接正常',
  clusterUnreachable: '无法连接',
  workloadsCount: '个工作负载',
  notReadyCount: '未就绪',
  noActivityYet: '暂无发布或构建记录。',

  res_deployed: '已更新',
  res_current: '已是最新',
  'res_would-deploy': '将会更新',
  res_paused: '已暂停',
  'res_argo-cd': 'Argo CD 托管',
  res_error: '失败',

  st_ok: '成功',
  st_partial: '部分成功',
  st_error: '失败',
  st_queued: '排队中',
  st_started: '构建中',
  st_skipped: '已跳过',

  kind_deploy: '全量发布',
  kind_rollout: '单服务发布',
  kind_rollback: '版本回滚',
  kind_build: '镜像构建',
  kind_snapshot: '集群快照',
  kind_pause: '自动发布开关',

  servicesTitle: '服务工作负载',
  servicesSubtitle: '按容器镜像仓库聚合展示跨集群工作负载与当前运行标签。',
  searchServicesPlaceholder: '按名称、命名空间或镜像搜索…',
  allClusters: '全部集群',
  tagsInUse: '运行标签',
  tagDrift: '版本不一致',
  readyLabel: '就绪',
  cronSuspended: '已挂起',
  lastRun: '上次运行',
  deployTagAction: '部署指定标签…',
  pauseAutoDeploy: '暂停自动发布',
  resumeAutoDeploy: '恢复自动发布',
  argoManagedHint: '由 Argo CD 托管，Ferry 自动跳过',
  rolloutModalTitle: '单工作负载部署标签',
  rolloutModalDesc: '仅更新当前选中的工作负载（即使已暂停自动发布也会生效）。',
  newTagLabel: '目标镜像标签',
  noServicesFound: '没有符合筛选条件的工作负载。',

  sourcesTitle: '代码源',
  sourcesSubtitle: '关联至 CNB 流水线构建或外部 CI 推送的 GitHub 仓库。',
  newSourceBtn: '添加代码源',
  editSourceTitle: '编辑代码源',
  newSourceTitle: '关联 GitHub 代码源',
  repoLabel: 'GitHub 仓库 (owner/name)',
  branchLabel: '监听分支',
  builderLabel: '构建方式',
  builderCNB: 'CNB 构建',
  builderExternal: '外部 CI',
  imageLabelCNB: '短镜像名',
  imageLabelExternal: '完整镜像仓库地址',
  buildArgsLabel: '构建参数 BUILD_ARGS（可选）',
  resolvedImageLabel: '产物镜像地址',
  buildNowBtn: '立即构建',
  buildingToast: '已加入构建队列',
  ciSnippetBtn: 'CI 接入示例',
  webhookOk: 'Webhook 已就绪',
  webhookMissing: '未配置 Webhook',
  webhookNoPerm: '需手动配置 Webhook',
  webhookChecking: '正在检查…',
  setupWebhookBtn: '一键配置 Webhook',
  manualWebhookTitle: '手动配置 GitHub Webhook',
  manualWebhookDesc: '当前 GitHub Token 无该仓库管理权限，请在仓库 Settings → Webhooks 中添加：',
  payloadUrlLabel: '回调地址 (Payload URL)',
  contentTypeLabel: '内容类型 (Content type)',
  eventsLabel: '触发事件',
  secretNote: 'Secret 请填写 ferry.yaml 中配置的 github.webhookSecret。',
  lastBuildLabel: '最近构建',
  buildLogsLink: '查看日志',
  noBuildsYet: '尚无构建记录',
  noSourcesYet: '暂未添加任何代码源。关联仓库后可追踪构建并跳转对应服务。',
  deleteSourceConfirm: '确认移除该代码源？集群中正在运行的工作负载不会受影响。',

  activityTitle: '操作动态',
  activitySubtitle: '按时间倒序记录所有镜像发布、单点部署、回滚、构建与快照事件。',
  allKinds: '全部类型',
  filterByImagePlaceholder: '按镜像仓库筛选…',
  rollbackBtn: '回滚此发布',
  rollbackModalTitle: '确认回滚版本',
  rollbackModalDesc: 'Ferry 会先校验目标容器当前镜像是否仍为该次发布后的标签，若已被后续发布修改将安全跳过。',
  confirmRollbackBtn: '确认执行回滚',
  detailsShow: '展开明细',
  detailsHide: '收起明细',

  settingsTitle: '系统设置',
  settingsSubtitle: '管理 CI 部署令牌、集群连接状态、GitOps 快照与流水线接入。',
  tokensSectionTitle: '部署令牌 (Deploy Tokens)',
  tokensSectionDesc: '供 GitHub Actions 或外部 CI 调用 POST /deploy 使用的 Bearer 令牌。',
  tokenNamePlaceholder: '令牌名称（例如 github-actions-prod）',
  createTokenBtn: '创建令牌',
  tokenCreatedWarning: '请立即复制保存此令牌——服务端仅存储 SHA-256 摘要，关闭后无法再次查看。',
  tokenColName: '名称',
  tokenColPrefix: '前缀',
  tokenColCreatedBy: '创建人',
  tokenColLastUsed: '最近使用',
  neverUsed: '从未使用',
  revokeBtn: '吊销',
  noTokensYet: '暂未创建部署令牌。',

  clustersSectionTitle: '受管集群',
  clustersSectionDesc: '通过 ferry.yaml 或 clustersFile 配置（只读展示）。',

  snapshotsSectionTitle: '集群配置快照',
  snapshotsSectionDesc: '清洗后的 Kubernetes 清单与 SOPS 加密 Secret 自动提交至 Git 仓库。',
  snapshotNowBtn: '立即执行快照',
  snapshottingBtn: '正在生成快照…',
  snapshotNotConfigured: '当前实例未配置快照 Git 仓库。',
  snapshotScheduleLabel: '每日定时计划',
  snapshotLastEvent: '最近一次快照',

  connectCISectionTitle: '接入外部 CI',
  connectCISectionDesc: '在任意 CI 流水线末尾通过一条 HTTP 请求触发多集群滚动更新。',
};

const dictionaries: Record<Locale, Messages> = {
  en,
  'zh-CN': zhCN,
};

interface I18nContextValue {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: Messages;
}

const I18nContext = createContext<I18nContextValue>({
  locale: 'en',
  setLocale: () => {},
  t: en,
});

function detectInitialLocale(): Locale {
  if (typeof window === 'undefined') return 'en';
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored === 'en' || stored === 'zh-CN') return stored;
  const nav = navigator.language || '';
  return nav.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
}

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(detectInitialLocale);

  const setLocale = (next: Locale) => {
    setLocaleState(next);
    localStorage.setItem(STORAGE_KEY, next);
    document.documentElement.lang = next;
  };

  return (
    <I18nContext.Provider value={{ locale, setLocale, t: dictionaries[locale] }}>
      {children}
    </I18nContext.Provider>
  );
}

export function useI18n() {
  return useContext(I18nContext);
}

export function formatRelativeTime(iso: string | undefined, locale: Locale): string {
  if (!iso) return '—';
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return iso;
  const diffSec = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (locale === 'zh-CN') {
    if (diffSec < 45) return '刚刚';
    if (diffSec < 3600) return `${Math.floor(diffSec / 60)} 分钟前`;
    if (diffSec < 86400) return `${Math.floor(diffSec / 3600)} 小时前`;
    return `${Math.floor(diffSec / 86400)} 天前`;
  }
  if (diffSec < 45) return 'just now';
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`;
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`;
  return `${Math.floor(diffSec / 86400)}d ago`;
}

export function greetingForNow(t: Messages, userName?: string): string {
  const h = new Date().getHours();
  const base = h < 12 ? t.greetMorning : h < 18 ? t.greetAfternoon : t.greetEvening;
  if (!userName) return base;
  const firstName = userName.split(/[\s@]/)[0];
  return t === zhCN ? `${base}，${firstName}` : `${base}, ${firstName}`;
}
