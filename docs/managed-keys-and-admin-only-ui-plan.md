# Managed-Key Bypass and Admin-Only UI Implementation Plan

Status: revised after independent architecture and security review.

Baseline: `haowang02/cpa-plugin-key-billing` commit
`78150bdc10ce28afde4b3539f1be48ae2bead995`.

## 1. Objective

Produce a maintained fork with two behavior changes:

1. A downstream API key without a subscription-plan binding bypasses plugin
   enforcement and accounting.
2. The plugin registers zero `/v0/resource/plugins/...` routes. Administrators
   use a separately released, self-contained UI served by Nginx, and every
   dynamic operation stays under CPA Management-key authentication.

The implementation remains a plugin-only fork. CLIProxyAPI host changes are
outside the initial release. Host limitations are documented explicitly rather
than hidden behind stronger guarantees.

## 2. Decisions fixed by review

The review resolved these design questions before coding:

1. **Current-state semantics:** each plugin callback evaluates the key's current
   plan binding. No request admission snapshot is implemented.
2. **Plan binding is the opt-in marker:** `PlanID == ""` means unmanaged.
3. **Database corruption remains fail-fast:** missing or malformed plans prevent
   plugin state from loading. The fork does not add per-request broken-plan
   recovery.
4. **Zero Resource routes:** the UI is a release artifact served directly by
   Nginx. Plugin-menu integration is removed.
5. **Self-contained UI:** no CDN scripts, fonts, styles, images, or dynamic
   imports.
6. **In-memory Management key:** standalone UI credentials are not persisted in
   localStorage, sessionStorage, or IndexedDB.
7. **No live plugin replacement:** deploy during a controlled CPA restart after
   traffic drain.
8. **Existing release matrix is retained:** Darwin, Linux, and Windows on amd64
   and arm64 remain built unless a later release explicitly narrows support.
9. **Home mode and competing scheduler plugins are unsupported for credential
   routing enforcement.** Deployment validation must detect these conditions.
10. **Managed enforcement inherits CPA's host fail-open behavior.** The initial
    fork is an operational quota and routing control, not a hard security or
    financial isolation boundary.

## 3. Scope

### Included

- Unmanaged-key bypass in request interception, scheduler handling, post-auth
  observation, and usage accounting.
- Existing plan binding as the only activation mechanism.
- Removal of downstream API-key account APIs and UI.
- Removal of all plugin Resource routes.
- Admin-only static UI artifact served by Nginx.
- Management endpoints retained under
  `/v0/management/plugins/cpa-key-billing/...`.
- Removal of remote UI dependencies and persistent browser credentials.
- Central no-store headers for Management responses.
- Updated unit, browser, integration, E2E, deployment, and performance tests.

### Excluded

- Stable admission-to-usage correlation across retries.
- CLIProxyAPI ABI or scheduler changes.
- Fail-closed host behavior after plugin errors, panic, or fusion.
- CPA Home-compatible credential routing.
- Composition with another scheduler plugin.
- Public downstream-user billing pages.
- Multi-instance quota consistency.
- Zero host-side plugin invocation overhead for unmanaged keys.

## 4. Compatibility baseline

- Plugin ID: `cpa-key-billing`
- Plugin ABI: `1`
- Plugin schema version: `4`
- SQLite schema version: `17`
- Existing state file: `plugins/cpa-key-billing-state-v1.db`
- Existing minimum CPA version remains `7.2.143` because the revised design does
  not depend on usage `RequestID`.

The fork preserves the existing database schema and can read the upstream
plugin's current state. Upstream and fork binaries cannot be installed together
because they share one plugin ID.

## 5. Management policy

### 5.1 Policy definition

Add a side-effect-free billing-layer lookup, preferably in
`internal/billing/management_policy.go`:

```go
type ManagementPolicy struct {
    Scope              string
    Managed            bool
    ConfigurationError string
}

func (s *Store) ManagementPolicy(scope string) ManagementPolicy
```

The method returns the canonical normalized scope and evaluates one state
snapshot under `RLock`.

| State | Policy |
| --- | --- |
| Blank scope | Unmanaged |
| Unknown key | Unmanaged |
| Deleted key | Unmanaged |
| Live key with empty `PlanID` | Unmanaged |
| Live key with valid non-empty `PlanID` | Managed |
| Legacy/external key with no deletion timestamp and valid plan | Managed |

A non-empty missing plan should never reach this method from a valid loaded
state. Existing SQLite load validation remains authoritative and fails plugin
configuration when plan or cycle references are invalid. As defense in depth,
an unexpected in-memory missing-plan reference returns `Managed: true` with a
configuration error so request admission terminates with a controlled `503`.
It never clears the binding or converts the key to unmanaged state.

Remove existing silent repair paths in key views and authorization. Add state
invariant validation after configuration mutations so future code cannot turn a
broken managed binding into an unmanaged pass-through.

### 5.2 Canonical scope

All downstream calls use `policy.Scope`, including:

- routing lookup
- scheduler pool lookup
- concurrency accounting
- quota authorization
- quota-block logging
- key/event lookup

This closes the existing inconsistency where some paths only trim scope while
others lowercase it.

### 5.3 Current-state transition semantics

Every callback reads current state independently:

- Unbinding before a callback makes that callback bypass plugin work.
- Binding before a callback makes that callback use managed behavior.
- A request may cross a bind/unbind boundary while in flight.

Operational rule: plan binding changes are administrative cutovers for new
traffic. Administrators should drain or pause affected keys before changing a
binding when exact accounting around the transition matters.

UI wording must say:

> Binding changes apply to subsequent plugin callbacks. In-flight requests may
> finish under mixed pre-change and post-change state.

## 6. Request path changes

### 6.1 Before-auth interception

Modify `internal/plugin/intercept.go`.

Required order:

1. Decode request.
2. Return when plugin is globally disabled.
3. Extract caller scope.
4. Resolve `ManagementPolicy`.
5. Return an empty successful interception response when unmanaged.
6. Return a controlled `503 subscription_configuration_error` when the policy
   reports an invariant violation.
7. Begin admission bookkeeping for a managed key.
8. Apply route, price, concurrency, and quota checks.

The existing `requestAdmission` structure remains synchronization bookkeeping
for races between request completion and slot creation. It is not a persistent
policy snapshot and does not correlate usage records.

The unmanaged return occurs before:

- `beginAdmission`
- `ResolveRouting`
- `beginRouteLog`
- `ResolveModelPrice`
- models.dev refresh
- `AcquireSlot`
- `Authorize`

This guarantees that an unmanaged key can request a model with no plugin price
and still reach CPA's native execution path.

### 6.2 Post-auth interception

Resolve current policy from caller scope before observing selected credentials.
Unmanaged requests return immediately and create no routing observations.

### 6.3 Scheduler

Modify `internal/plugin/scheduler.go` so policy resolution occurs before
`observeCandidates` and before routing resolution.

Behavior:

- unmanaged: `{Handled: false}`
- managed without credential restrictions: `{Handled: false}`
- managed with credential restrictions: existing filtered weighted scheduling

This removes plugin candidate-inventory mutation for unmanaged keys.

The host still switches every non-Home request to CPA's legacy scheduler path
when any plugin scheduler is registered. This affects unmanaged traffic too.
Performance tests must include candidate counts of 1, 10, 100, and 1,000 and
must include retry/failover selection.

### 6.4 Completion cleanup

Completion cleanup never depends on current management policy.

Always:

- call `ReleaseSlot(requestID)`
- remove or finish any pending route-log record
- mark any in-flight admission bookkeeping complete

This prevents slot or log leaks when a key is unbound, the plugin is disabled,
or configuration changes while a request is running.

A route log created while the key was managed is emitted on completion even if
the key was unbound in the meantime. It describes the historical admission
decision. Unmanaged admissions never create route logs.

### 6.5 Usage handling

Modify `handleUsage` before token parsing, cost calculation, price lookup, event
construction, and credential observation.

- unmanaged: return success immediately
- managed: preserve current usage behavior

Unmanaged usage must not call:

- `usageBreakdown`
- `ResolveModelPrice`
- `RecordUsage`
- `RecordUsageError`
- `observeCredentialUsage`

The fork accepts current-state race semantics. A request admitted as managed can
become unmanaged before asynchronous usage delivery and therefore produce no
plugin event. A request admitted unmanaged can become managed before usage and
therefore be recorded. Deployment documentation must state this limitation.

### 6.6 External access-provider principals

The fork stops automatic key creation from unmanaged traffic. Keys supplied by
other frontend auth providers therefore need an explicit inventory/sync path
before administrators can bind a plan.

Initial support is limited to keys available through CPA's configured
`api-keys` management list and legacy plugin records already carrying a valid
plan. New external-provider key enrollment is deferred.

## 7. Persistence behavior

No SQLite migration is required.

Unmanaged traffic writes none of these:

- request events
- request errors
- quota cycles
- route debug logs
- credential observations derived from traffic

Administrative key synchronization may still store:

- salted caller-scope digest
- masked preview
- label
- `InConfig` state

Plaintext downstream keys remain excluded from SQLite, WAL, SHM, and plugin
logs.

Plan corruption remains a startup/reconfiguration error. Add tests for:

- missing referenced plan row
- malformed plan data
- invalid quota cycle
- unsupported schema version

Do not add silent per-request repair or automatic unbinding for corrupt loaded
state.

## 8. Resource-boundary remediation

### 8.1 Register zero Resource routes

Modify `internal/plugin/management.go` so
`ManagementRegistrationResponse.Resources` is empty.

Remove all registration and dispatch for:

```text
/ui
/profile
/subscription
/routing
/prices
/analysis
/events
/errors
/auth-files
/auth-files/quota
/auth-files/quota/reset
```

Delete:

- `resourceEndpoints`
- `routeResource`
- API-key Resource authentication
- Resource quota-reset workaround
- UI Resource response and CSP construction

Every request under `/v0/resource/plugins/cpa-key-billing/...` must receive
`404` from CPA.

### 8.2 Preserve Management routes

Keep administrator features under:

```text
/v0/management/plugins/cpa-key-billing/*
```

Preserve:

- key inventory and labels
- plan management and quota resets
- route management
- credential inventory and synchronization
- prices and reference-price refresh
- events, errors, analysis, and plugin logs
- auth-file inventory
- upstream quota lookup
- Codex quota reset via POST

CPA Management middleware remains the authorization boundary.

### 8.3 Explicit admin-only handlers

Replace the shared zero-value `viewAccess{}` convention with explicitly named
admin handlers, for example:

- `listAdminRequestEvents`
- `listAdminRequestErrors`
- `adminAnalysis`
- `listAdminAuthFiles`
- `adminAuthQuota`
- `adminAuthQuotaReset`

No Resource registration may reference an admin handler. Add a CI test that
fails when any Resource route is registered or any legacy menu metadata would
cause CPA to convert a Management route into a Resource route.

### 8.4 Privileged callbacks

`host.auth.list`, `host.auth.get`, and `host.http.do` remain available only from
Management-authenticated admin flows and internal managed request processing.
With zero Resource handlers, there is no unauthenticated plugin call path to
these callbacks.

The host ABI still gives the loaded plugin process callback capability. The
fork's source and registration tests enforce the boundary; a future CLIProxyAPI
capability-scoping change would provide stronger host enforcement.

## 9. Account portal removal

Remove downstream API-key account functionality from:

- `internal/plugin/account.go`
- API-key branches in `internal/plugin/view.go`
- `internal/plugin/auth_files.go`
- `internal/plugin/ui.html`
- account tests
- E2E account requests

Remove:

- Bearer API-key Resource authentication
- profile/subscription/routing account responses
- account events/errors/analysis
- account auth-file and quota views
- API-key-initiated Codex quota reset
- account email masking
- account role and hash routing

Keep these YAML fields accepted as deprecated no-ops for one compatibility
cycle:

```yaml
mask_api_key_view_emails: false
allow_api_key_quota_reset: false
```

Strict YAML decoding makes compatibility parsing necessary. Tests must prove
both fields parse but enable no route, UI control, masking, reset permission, or
callback.

## 10. Management response security

Centralize Management response headers in `internal/plugin/envelope.go` or a
new admin response constructor:

```text
Cache-Control: private, no-store
Pragma: no-cache
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
```

Apply them to successful and error responses containing:

- keys and previews
- plans and routes
- prices
- events and errors
- analysis
- plugin logs
- auth-file inventory
- upstream quota data

Test headers through CPA's HTTP server, not only direct plugin handler calls.

## 11. Standalone admin UI artifact

### 11.1 Packaging

Keep the UI source in the repository, but stop embedding/serving it from the
plugin runtime.

Add a deterministic UI assembly command, for example:

```text
cmd/build-ui
```

It combines:

- `internal/plugin/ui.html`
- `internal/plugin/i18n.js`
- locale JSON
- vendored chart/rendering code when still required

Release output:

```text
cpa-key-billing-ui.html
```

The release workflow publishes the UI file and includes it in `checksums.txt`.

### 11.2 UI simplification

The UI contains one admin role.

Remove:

- `ACCOUNT_BASE`
- account role switching
- API-key login
- `#account` routes
- account pages and state
- account IndexedDB/session credentials
- embedded management-panel assumptions tied to Resource menu loading

The standalone Management key stays in JavaScript memory for the page lifetime.
Reload and logout clear it.

### 11.3 Supply-chain requirements

The released UI must use no remote executable or display dependency:

- no jsDelivr
- no remote Chart.js
- no remote fonts
- no external styles
- no dynamic import
- no remote image

Use vendored inline code or native HTML/SVG charts.

Nginx CSP target:

```text
default-src 'none';
script-src 'unsafe-inline';
style-src 'unsafe-inline';
connect-src 'self';
img-src data:;
base-uri 'none';
form-action 'none';
frame-ancestors 'none';
object-src 'none';
```

A later split-file build may replace inline scripts with hashes or nonces.

Add CI checks rejecting external URLs and dynamic script injection in the
released artifact.

### 11.4 Key synchronization limitation

The current admin UI obtains CPA's configured API-key list through authenticated
Management APIs and sends those keys to the plugin sync endpoint, which hashes
and masks them before persistence. The initial plugin-only fork retains this
flow.

Risk controls:

- self-contained trusted UI code
- TLS
- in-memory Management key
- no browser credential persistence
- no request-body logging on Management paths
- tests scanning DB/WAL/SHM/logs for known plaintext test keys

A future CLIProxyAPI enhancement should derive scope and preview server-side and
send only safe identities to the plugin.

## 12. Nginx deployment

Serve the checked release artifact directly from Nginx under an admin-only path.
Example:

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

Requirements:

- TLS is mandatory for Basic Auth.
- VPN or source-IP ACL is preferred where available.
- CPA Management authentication remains enabled independently.
- CPA backend is reachable only from Nginx or trusted management networks.
- Docker networks are private; unrelated containers cannot reach CPA directly.
- Host-published CPA ports stay bound to loopback.

Test exact, trailing-slash, doubled-slash, percent-encoded, case-variant, and
non-GET requests through Nginx and directly against every reachable backend
address.

## 13. Host limitations and supported deployment

### 13.1 Fail-open behavior

Current CLIProxyAPI logs and ignores request-interceptor errors. A plugin panic
can fuse the plugin, after which interception and scheduling are skipped. The
fork cannot provide hard fail-closed enforcement without a CLIProxyAPI host
change.

Production controls:

- alert on plugin load, RPC, panic, fusion, and database-write errors
- health-check plugin registration after every restart
- keep CPA and plugin versions pinned
- use controlled restart deployment
- treat quotas as operational limits rather than a payment-grade hard boundary

A future host project may add per-plugin fail-closed enforcement.

### 13.2 Home mode

CPA Home selection runs before plugin scheduler selection. Provider/credential
restrictions are therefore unsupported when Home mode is enabled.

Deployment must verify Home is disabled for this CPA instance.

### 13.3 Competing schedulers

CPA selects one active scheduler plugin by priority. A competing scheduler can
prevent this fork from enforcing credential/provider restrictions.

Deployment must verify this fork is the only active scheduler plugin.

Add `scripts/preflight_deployment.sh` to inspect the effective CPA YAML before
deployment. It must fail when Home is enabled, plugin loading is disabled, this
plugin instance is disabled, or another configured plugin is known to register
a scheduler. Because CPA does not expose a complete scheduler inventory through
the current plugin API, the script supplements config inspection with a startup
log/Management status check and treats unknown scheduler plugins as an operator
review item.

### 13.4 Hot replacement

The plugin has no quiesce implementation. Live replacement can lose in-memory
concurrency slots and pending lifecycle state.

Deployment procedure:

1. stop or drain incoming traffic
2. wait for active model requests to complete
3. stop CPA
4. replace plugin library and UI artifact
5. start CPA
6. verify plugin registration and Management endpoints
7. resume traffic

## 14. Backend/UI status model

Extend `KeyView` or the admin key-row response with an explicit status enum:

```text
unmanaged
managed
deleted
external-managed
```

Status precedence:

1. deleted
2. managed with `InConfig == false` → external-managed
3. managed
4. unmanaged

The UI shows:

- Unmanaged: no plan, complete plugin bypass
- Managed: plan bound, plugin controls active
- External managed: legacy/non-config principal with a valid plan
- Deleted: absent from CPA configuration

For unmanaged keys, route and concurrency settings are disabled or marked
inactive. Unbinding warns that route, concurrency, price, quota, and accounting
behavior stop on subsequent callbacks.

## 15. Test plan

### 15.1 Policy tests

Cover:

- blank scope
- unknown scope
- deleted key
- configured unbound key
- configured managed key
- external managed key
- mixed-case scope normalization
- corrupt plan/cycle load failure

### 15.2 Interceptor tests

Unmanaged key:

- passes an unpriced model
- ignores stale model allow/deny settings
- performs no reference-price download
- acquires no concurrency slot
- activates no quota cycle
- creates no route log

Managed key:

- denied model returns `403`
- exhausted quota returns `429`
- missing price returns `503`
- concurrency limit remains enforced

Completion tests:

- managed admission followed by unbind still releases slot
- global plugin disable before completion still releases slot
- duplicate completion is harmless
- pending route logs are removed on every completion path

### 15.3 Scheduler tests

Use dedicated fixtures:

- `newUnmanagedApp`
- `newManagedApp`
- `newUnknownKeyApp`
- `newDeletedKeyApp`

Verify:

- unmanaged returns `Handled: false` before candidate observation
- managed unrestricted returns `Handled: false`
- managed restricted filters candidates
- provider/category denial works
- retries retain expected allowed-candidate behavior

Add host integration checks for unsupported configurations:

- Home enabled
- higher-priority scheduler plugin
- lower-priority scheduler plugin

These tests document behavior and ensure deployment validation catches it.

### 15.4 Usage tests

Unmanaged usage:

- creates no traffic-only key
- creates no normal event
- creates no error event
- changes no cycle
- performs no price lookup
- performs no credential observation

Managed usage remains correct for:

- successful request
- failed request
- token accounting variants
- custom, built-in, and reference price

Add bind/unbind race tests that assert the documented current-state semantics.

### 15.5 Resource-boundary tests

Registration must contain zero Resources.

All previous Resource paths return `404`, with and without a downstream API key:

```text
/ui
/profile
/subscription
/routing
/prices
/analysis
/events
/errors
/auth-files
/auth-files/quota
/auth-files/quota/reset
```

Add a source/registration guard that fails if any Resource is added.

### 15.6 Management authorization tests

Through CPA HTTP:

- missing Management key fails
- invalid Management key fails
- valid Management key succeeds
- every mutating endpoint remains authenticated
- auth-file list/quota/reset remain authenticated
- Codex reset is POST-only
- successful and error responses carry no-store headers

### 15.7 UI tests

Run desktop and narrow-screen browser tests on port `18765`.

Verify:

- only admin mode exists
- no Resource URL is requested
- no account UI exists
- no remote network request occurs except same-origin Management APIs
- Management key stays out of browser persistent storage
- logout clears in-memory authenticated state
- all admin tabs load
- managed/unmanaged status and controls render correctly

Run:

```bash
npm ci --prefix scripts
node scripts/format_ui.mjs
node scripts/format_ui.mjs --check
npm test --prefix scripts
```

### 15.8 E2E rewrite

Rewrite `scripts/e2e_cpa_billing.sh` into two explicit phases.

Unmanaged phase:

- sync key without binding a plan
- send successful, failed, priced, and unpriced requests
- verify zero plugin events and zero quota state
- verify native credential selection

Managed phase:

- create and bind a high-limit plan before billing, routing, concurrency, and
  quota tests
- retain existing managed behavior assertions
- replace account Resource calls with direct `404` assertions

Run against `v7.2.143` and the actual VPS CPA version.

### 15.9 Performance tests

Compare:

1. plugin disabled
2. fork enabled, unmanaged key
3. fork enabled, managed key without credential restrictions
4. fork enabled, managed key with credential restrictions

Measure with 1, 10, 100, and 1,000 eligible credentials where practical, plus
retry/failover scenarios.

Collect:

- RPS
- p50/p95/p99 admission latency
- TTFT and total latency
- CPU and RSS
- SQLite transaction rate
- DB/WAL growth
- usage queue/memory growth
- credential choice sequence

Initial release gates on the target VPS and production credential count:

- unmanaged-key p95 admission overhead at most 2 ms versus plugin disabled
- unmanaged-key p99 admission overhead at most 5 ms
- sustained throughput loss at most 10%
- CPU increase at most 10% at the same request rate
- no sustained RSS growth after warm-up
- zero unmanaged request-event and quota writes

If the target VPS cannot meet a gate, deployment uses separate CPA instances for
managed and unmanaged keys or the release is held pending host optimization.

## 16. Release and deployment

### 16.1 Fork metadata

Update:

- GitHub repository metadata
- plugin version
- installer repository URL
- README links
- release notes

Keep the existing cross-platform build matrix initially.

### 16.2 Release artifacts

Publish:

- dynamic plugin libraries for the existing matrix
- `cpa-key-billing-ui.html`
- `checksums.txt`

Verify every archive and standalone UI checksum in CI.

### 16.3 Secret and file hygiene

Deployment steps include:

```bash
chmod 0600 config.yaml
chmod 0600 auth*.json
chmod 0700 auths plugins
```

Adapt paths to the actual deployment owner and container mounts. Add secret
scanning before releases and ensure root-level auth files are excluded from
version control and backups shared outside the operator boundary.

Tests scan the state DB, WAL, SHM, plugin logs, and released UI/browser stores
for known plaintext test keys.

### 16.4 Backup and rollback

Take a quiescent backup after stopping CPA, or use SQLite's backup API. Avoid
copying an active WAL database as an informal backup.

Because schema version remains 17, normal rollback restores:

1. previous plugin binary
2. previous Nginx UI configuration/artifact
3. previous plugin configuration when changed

Keep the current database by default. Restore the database only for confirmed
corruption or an actual incompatible format change.

Add bidirectional compatibility tests:

1. upstream opens and writes a v17 DB
2. fork opens and writes managed data
3. upstream reopens the same DB
4. plans, keys, cycles, routes, prices, and events remain valid

Rolling back to upstream restores upstream semantics, including accounting and
price checks for unbound keys.

## 17. Implementation phases

### Phase 1: Baseline and fork setup

- configure `origin` and read-only `upstream`
- create feature branch
- run untouched tests and E2E
- pin baseline SHA

### Phase 2: Managed-key bypass

- add canonical `ManagementPolicy`
- add early interceptor bypass
- move scheduler bypass before candidate observation
- add usage bypass
- preserve unconditional completion cleanup
- add dedicated managed/unmanaged fixtures and tests

### Phase 3: Resource and account removal

- register zero Resources
- remove dynamic Resource dispatch
- remove account API and quota-reset branches
- refactor explicit admin handlers
- centralize no-store headers
- add authorization and source guards

### Phase 4: Standalone admin UI

- remove account mode and persistent credential stores
- remove external dependencies
- add managed status UX
- add deterministic UI builder and release artifact
- run formatter and browser regression suite

### Phase 5: E2E, performance, and compatibility

- rewrite billing E2E phases
- test supported and unsupported scheduler configurations
- run DB forward/backward compatibility
- run sustained load matrix

### Phase 6: VPS canary

- deploy to staging with Home disabled and no competing scheduler
- serve UI through protected Nginx path
- reject complete plugin Resource prefix
- test unmanaged and managed keys
- inspect plugin errors, events, DB growth, CPU, RSS, and latency
- deploy through traffic drain and CPA restart

## 18. Acceptance criteria

1. Unmanaged unpriced requests pass plugin interception.
2. Unmanaged requests receive no plugin route, price, concurrency, or quota
   decision.
3. Unmanaged usage creates no event, error, cycle, or credential observation.
4. Managed behavior remains compatible with upstream for model, credential,
   quota, price, and concurrency controls.
5. Completion always releases managed state after bind/unbind or disable changes.
6. Plugin registration exposes zero Resource routes.
7. Every former Resource business path returns `404`.
8. Privileged auth-file and HTTP callbacks are reachable only from authenticated
   Management flows.
9. The released UI is self-contained and served only by protected Nginx.
10. The UI persists no Management key or downstream API key.
11. Management success and error responses carry no-store security headers.
12. Existing valid v17 state loads without migration.
13. Corrupt plan/cycle state fails plugin loading with a clear operator error.
14. Home mode and competing scheduler configurations are rejected by deployment
    validation or clearly reported unsupported.
15. Unit, browser, integration, E2E, compatibility, and load tests pass.
16. Sustained unmanaged traffic causes no request-event DB growth.
17. Release artifacts and standalone UI have published checksums.

## 19. Residual risks

The accepted residual risks are:

- host fail-open behavior after plugin error, panic, or fusion
- global legacy scheduler overhead for unmanaged requests
- current-state bind/unbind races for in-flight requests
- plaintext configured API keys entering the trusted admin UI during sync
- host callback capability remaining process-wide inside a trusted loaded plugin
- single-process SQLite accounting and no cross-instance quota consistency

These risks require CLIProxyAPI host changes for full removal. They remain
visible in release documentation and operational monitoring.
