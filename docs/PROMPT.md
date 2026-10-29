# 给实现方 AI 的提示词

（整段复制给对方。它需要能读写本仓库并在本地运行命令；不需要、也没有任何服务器访问权限。）

---

你要在这个仓库里从零实现 **Ferry**：一个 Go 单二进制 + 内嵌 React SPA 的开源服务。CI 构建出镜像后告诉它 `image:tag`，它把多个 Kubernetes 集群里正在跑这个镜像的工作负载更新到新 tag，并跳过 Argo CD 管理的对象；同时提供一个 Claude.ai 风格的网页，用来管理构建源、查看哪里跑着什么、部署指定版本、回滚、暂停自动部署。

## 开始之前

1. 完整读一遍 `docs/DESIGN.md`。它就是需求规格，里面写明的行为、接口、配置格式都按它来，不要自行改动。
2. 读 `docs/reference/`：
   - `relay.py` 是线上正在跑的旧版（Python），后端行为以它为准，要逐项移植；
   - `bump.sh` 和 `pipeline.yml` 是已经在调用它的外部方，接口不能变；
   - `ci-deployer.yaml` 是它在各集群里拥有的全部权限。
3. `internal/kube/kube.go` 已经写好（最小 REST 客户端），直接用，需要时扩展。**不要引入 client-go。**

## 硬性约束

- **依赖**：
  - Go 只允许标准库、`sigs.k8s.io/yaml`、`github.com/coreos/go-oidc/v3`、`golang.org/x/oauth2`。
  - 前端只允许 React、react-router、lucide-react 和 Vite/TypeScript 工具链。
  - 想加别的，先停下来说明理由。
- **兼容性**：DESIGN.md 里 "Compatibility" 一节列出的每一条都必须保持原样，并且每条都要有测试覆盖。这些接口正被约 10 个 CI 流水线和 GitHub webhook 调用。
- **权限**：只能用到 `get`、`list`、`patch`，没有 `watch`。任何功能都不能依赖超出这些的权限。
- **无法连真实集群**：
  - 所有开发和测试都基于你自己写的 `internal/kube/fake`（httptest 假 API Server）和 `ferry -demo` 模式。
  - 测试里不许访问外网，GitHub、CNB、git 服务器、OIDC issuer 全部用 httptest 假服务。
  - snapshot 测试可以用本地临时 bare 仓库。
  - 测试环境里如果找不到 sops，就跳过加密相关的那一步，并且要明确标注是跳过而不是通过。
- **安全**：
  - 严格遵守 DESIGN.md 的 "Security checklist"。
  - 仓库里不得出现任何真实 token、密钥或内网 IP。
  - 测试里用的假 token 要一眼能看出是假的。
- **UI 风格**：照 DESIGN.md 的 "UI" 一节实现 Claude.ai 的视觉语言，包括暖白底、衬线大标题、赭橙强调色、细边框、大留白、首页的输入框式部署入口。
  - 不要使用 Anthropic 的 logo 或字体文件。
  - 要做中英双语和明暗两种主题。
- **代码风格**：
  - 写得朴素、扁平、可读，不要过度抽象。
  - 注释只解释"为什么"。
  - 错误信息要对运维有用。

## 建议的工作顺序（每步一个或几个提交）

1. 搭好骨架：`go.mod`、`cmd/ferry`、配置加载与校验（含 `${VAR}` 展开、`clustersFile`、`seedSources`），以及 `internal/kube/fake`。
2. state 存储：ConfigMap 后端（带 resourceVersion 冲突重试）和文件后端，加上数据模型与测试。
3. 部署核心：Deploy（含 dry-run）、Rollout、Rollback、Pause/Resume、工作负载清单（带缓存），加上完整测试。
4. HTTP 层：
   - 先做旧接口 `POST /deploy`、`POST /`、`POST /snapshot`；
   - 再做 CI 认证（deploy token 和 gitRead，含缓存）、OIDC 登录（PKCE、state、nonce、组校验、签名 cookie）；
   - 最后做全部 `/api`，以及 SPA 托管与 fallback。
5. CNB 构建（mirror 和 start build，队列加 2 个 worker）、GitHub webhook 校验与 hook 管理、构建与部署事件的关联。
6. 快照：移植 relay.py 的 `snapshot()`，按新目录布局，迁移旧文件，并写出 `ferry.json`。
7. `-demo` 模式：起 2 到 3 个预置真实感数据的假集群。
8. 前端：设计变量（CSS 变量）、布局（侧栏）、Home（问候语、部署输入框、预览确认）、Services、Sources、Activity、Settings、i18n、主题。
9. 打包：Dockerfile、Makefile、GitHub Actions（ci 和 release）、`deploy/` 示例清单、README（含 demo 截图）、LICENSE（Apache-2.0）。

## 提交与交付

- 小步提交，提交信息用英文祈使句。作者设置为：

  ```
  git config user.name Exmeaning
  git config user.email mail@exmeaning.com
  ```

  如果环境变量 `GIT_AUTHOR_*` / `GIT_COMMITTER_*` 存在，会覆盖上面的设置，需要先 unset 或显式导出。
- 每个阶段结束都跑：`go vet ./...`、`go test -race ./...`、`cd web && npm ci && npm run build`。全部通过才能进入下一步。
- 完成后用中文给出一份交付说明，包括：
  - 实现了什么；
  - 与 DESIGN.md 有出入的地方，以及原因；
  - 已知限制与 TODO；
  - 怎么运行 demo；
  - 测试覆盖了哪些兼容性条目；
  - 哪些东西你无法验证、需要在真实环境里确认。最后这一项尤其重要，要写具体：比如 Zeabur Identity 的真实 token 格式、CNB 接口的真实响应、sops 在镜像里能否正常加密、真实集群的 RBAC 是否够用。
- 不要部署任何东西，不要改动本仓库以外的任何地方。部署、注册 OIDC 客户端、切换线上流量都由审查方完成。
