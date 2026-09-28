# 部署指南（cpa-key-billing 分支版）

本分支与上游 `haowang02/cpa-plugin-key-billing` 的两个行为差异决定了部署方式：

1. **未绑定订阅计划的下游 API key 是非托管 key**：计费插件完全绕过，模型、价格、并发、配额与记账均不适用，请求直接走 CLIProxyAPI 原生路径。
2. **插件注册零 Resource 路由**：管理界面是独立发布的单文件 UI，由 Nginx 在管理员专属路径下直接提供；所有管理写操作都在 CPA Management key 认证之下。唯一的例外是模型目录发现：UI 通过 `/v1/models` 读取模型列表，该接口属于下游客户端接口，需要 `api-keys` 中的任意一个 key。这要求管理员浏览器能够访问客户端接口，且该 key 会出现在浏览器请求中。

## 1. 部署前预检

```bash
scripts/preflight_deployment.sh <CPA 生效配置 config.yaml> \
  [--log-file <CPA 启动日志>] [--process <CPA 进程匹配模式>] \
  [--allow-plugin <插件ID>]... [--home-disabled]
```

预检会在以下情况失败：

- `plugins.enabled` 不为 `true`（动态插件加载被关闭）；
- `plugins.configs.cpa-key-billing.enabled` 不为 `true`（本插件实例被停用或缺失）；
- 检测到 Home 模式：启动日志出现 `Home mode`、预检自身环境存在 `HOME_JWT`、或 CPA 进程带 `-home-jwt`/`HOME_JWT`；
- 无法确认 Home 模式已关闭且未传入 `--home-disabled`；
- 存在其他已启用的插件实例，且未用 `--allow-plugin <id>` 逐个确认。

预检会在以下情况警告：

- 其他插件实例已通过 `--allow-plugin` 确认（CLIProxyAPI 只采用一个调度器插件，仍需人工确认其不注册调度器）；
- 已按 `--home-disabled` 声明 Home 关闭、但无法从进程或日志独立验证；
- 状态数据库尚不存在（首次启动会创建）。

`--home-disabled` 用于无法检查进程的受控重启窗口；它只是运维声明，不能推翻已经观察到的 Home 证据。非 Linux 环境没有 `/proc`，需要依赖 `--log-file` 或 `--home-disabled`。

提供 `--log-file` 时，预检还会确认日志中存在本插件的注册记录，且没有插件 panic 或熔断记录。

## 2. 管理界面的 Nginx 配置

UI 是一个自包含的 `cpa-key-billing-ui.html`，无任何远程脚本、字体、样式或图片依赖。由 Nginx 在管理员专属路径下直接提供：

```nginx
location = /admin/cpa-key-billing {
    auth_basic "CPA Billing Admin";
    auth_basic_user_file /etc/nginx/.htpasswd-cpa-billing;

    alias /srv/cpa-key-billing/cpa-key-billing-ui.html;
    default_type text/html;

    add_header Cache-Control "private, no-store" always;
    add_header Pragma "no-cache" always;
    add_header Referrer-Policy "no-referrer" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header Content-Security-Policy "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'" always;

    limit_except GET { deny all; }
}

location = /admin/cpa-key-billing/ {
    return 404;
}

location = /v0/resource/plugins/cpa-key-billing {
    return 404;
}

location ^~ /v0/resource/plugins/cpa-key-billing/ {
    return 404;
}
```

要求：

- Basic Auth 必须走 TLS；
- 有条件时叠加 VPN 或来源 IP ACL；
- CPA Management 认证独立于 Basic Auth 依然生效（两层认证都要通过）；
- CPA 后端只允许 Nginx 或可信管理网访问，Docker 网络保持私有，CPA 对外端口绑定 loopback。

上线前用精确路径、尾斜杠、双斜杠、百分号编码、大小写变体和非 GET 请求，穿过 Nginx 并直连每个可达后端地址逐一验证 Resource 前缀返回 404。

## 3. 发布物与校验

每次发布包含：

- 现有跨平台矩阵的动态插件库（darwin/linux/windows × amd64/arm64）；
- `cpa-key-billing-ui.html`；
- 覆盖上述全部文件的 `checksums.txt`。

UI 构建是确定性的：`go run ./cmd/build-ui` 对相同源码产出逐字节一致的文件。

## 4. 部署流程（无热替换）

插件没有静默切换实现，直接替换会丢失内存中的并发槽位和待完成状态。按以下顺序部署：

1. 停止或引流上游流量；
2. 等待在途模型请求全部完成；
3. 停止 CPA；
4. 替换插件库文件与 UI 发布物；
5. 启动 CPA；
6. 验证插件注册与 Management 端点（用 `preflight_deployment.sh --log-file` 复查）；
7. 恢复流量。

## 5. 备份与回滚

- 备份：停止 CPA 后做静态备份，或使用 SQLite backup API。不要在运行中直接复制 WAL 数据库。
- 数据库 schema 版本保持 17，与上游互通。回滚到上游时，按 1→4 替换回上游插件库与 Nginx UI 配置即可，数据库默认保留，仅在确认损坏或格式不兼容时才恢复备份。
- 回滚到上游即恢复上游语义：**未绑定计划的 key 会被上游记账并执行价格检查**，这是行为差异，不是数据损坏。

## 6. 文件与密钥规范

```bash
chmod 0600 config.yaml
chmod 0600 auth*.json
chmod 0700 auths plugins
```

- 不要把任何 auth 文件、状态数据库或配置放进版本控制或流出运维边界的备份；
- Management key 只保存在浏览器内存中，UI 刷新或退出即清除，不进入 localStorage/sessionStorage/IndexedDB；界面偏好使用固定命名空间，不含任何由 Management key 派生的取值；
- 模型目录发现会使用 `api-keys` 中的第一个 key 请求 `/v1/models`，请把它视为管理端凭据并保持 TLS；
- 管理响应带 `Cache-Control: private, no-store` 等响应头，Nginx 不应缓存 `/v0/management/` 路径。

## 7. 已知限制（运维视角）

- **主机侧 fail-open**：CPA 对插件错误只记录不阻断；插件 panic 会熔断。配额是运营控制，不是支付级硬边界。对插件加载、RPC、panic、熔断、数据库写入错误建立告警；每次重启后健康检查插件注册状态；版本钉住。
- **Home 模式**：Home 选择先于插件调度，凭据路由限制不生效。本部署要求 Home 关闭。
- **竞争调度器**：CLIProxyAPI 按优先级只采用一个调度器插件。本部署要求本插件是唯一启用的调度器插件。
- **绑定切换的当前状态语义**：绑定/解绑对"后续回调"生效，在途请求可能混合变更前后的状态。切换绑定前先排空或暂停受影响的 key。管理界面在解绑时会提示该行为。
- **单实例记账**：SQLite 单进程记账，不做跨实例配额一致性。
