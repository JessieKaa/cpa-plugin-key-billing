package plugin

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

const bypassKey = "sk-bypass-test-0001"

// A synced key without a plan binding: unmanaged by policy.
func newUnmanagedApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newAppWithPrice(t, true)
	if _, errSync := app.store.SyncKeys([]string{bypassKey}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	return app, billing.CallerScope(bypassKey)
}

func newManagedApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newConfiguredApp(t)
	return app, manageKey(t, app, bypassKey)
}

// A key the plugin has never seen: unmanaged by policy.
func newUnknownKeyApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newAppWithPrice(t, true)
	return app, billing.CallerScope("sk-never-seen-00001")
}

// A key that left CPA's configuration: unmanaged by policy.
func newDeletedKeyApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newAppWithPrice(t, true)
	if _, errSync := app.store.SyncKeys([]string{bypassKey}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	if _, errSync := app.store.SyncKeys([]string{}, true); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}
	return app, billing.CallerScope(bypassKey)
}

func interceptBypass(t *testing.T, app *App, scope, model, requestID string) RequestInterceptResponse {
	t.Helper()
	raw, errHandle := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: requestID, SourceFormat: "openai", Model: model, RequestedModel: model,
		Metadata: map[string]any{
			MetadataCallerScope: scope, MetadataRequestPath: "/v1/chat/completions", MetadataGenerate: true,
		},
	}))
	if errHandle != nil {
		t.Fatalf("request.intercept_before error = %v", errHandle)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

// An unmanaged key requests a model with no plugin price and still passes: no
// route, price, concurrency, or quota decision applies to it.
func TestUnmanagedInterceptBypassesAllEnforcement(t *testing.T) {
	app, scope := newUnmanagedApp(t)
	if _, errCreate := app.store.CreateRoute(billing.Route{
		Name: "Restrictive", Rule: billing.RouteRule{DeniedModels: []string{"mystery-model"}},
	}, []string{scope}); errCreate != nil {
		t.Fatalf("CreateRoute error = %v", errCreate)
	}
	if errSet := app.store.SetConcurrencyLimit(scope, 1); errSet != nil {
		t.Fatalf("SetConcurrencyLimit error = %v", errSet)
	}

	for _, requestID := range []string{"unmanaged-1", "unmanaged-2"} {
		response := interceptBypass(t, app, scope, "mystery-model", requestID)
		if response.Terminate {
			t.Fatalf("unmanaged request was terminated: %+v", response)
		}
	}
	view, _ := app.store.KeyViewForScope(scope)
	if view.CurrentConcurrency != 0 || len(view.Windows) != 0 {
		t.Fatalf("unmanaged admission touched quota or concurrency state: %+v", view)
	}
	for _, requestID := range []string{"unmanaged-1", "unmanaged-2"} {
		completeRequest(t, app, requestID)
	}
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Levels: []billing.PluginLogLevel{billing.PluginLogDebug}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if strings.HasPrefix(entry.Message, "route ") {
			t.Fatalf("unmanaged admission created a route log: %s", entry.Message)
		}
	}
	if len(app.admissions) != 0 {
		t.Fatalf("unmanaged admission left bookkeeping behind: %d markers", len(app.admissions))
	}
}

// The bypass runs before any reference-price download: admission of an
// unmanaged key never waits on the models.dev refresh.
func TestUnmanagedInterceptNeverWaitsForReferencePrices(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	app := newApp(billing.NewStore(openRepository, func(ctx context.Context) ([]byte, error) {
		close(entered)
		select {
		case <-release:
			return nil, context.Canceled
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	t.Cleanup(app.Shutdown)
	t.Cleanup(func() { close(release) })
	cfg := billing.DefaultConfig()
	cfg.Enabled = true
	cfg.StateFile = filepath.Join(t.TempDir(), "state.db")
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	// Leave startup reference loading pending; an unmanaged admission must not
	// touch the pending refresh at all.
	if _, errSync := app.store.SyncKeys([]string{bypassKey}, false); errSync != nil {
		t.Fatalf("SyncKeys error = %v", errSync)
	}

	response := interceptBypass(t, app, billing.CallerScope(bypassKey), "gpt-4o", "unmanaged-no-refresh")
	if response.Terminate {
		t.Fatalf("unmanaged request was terminated: %+v", response)
	}
	select {
	case <-entered:
		t.Fatal("unmanaged admission triggered a reference price download")
	default:
	}
}

// Unknown and deleted keys are unmanaged too: each policy state bypasses
// before-auth enforcement.
func TestUnknownAndDeletedKeysBypassIntercept(t *testing.T) {
	for _, test := range []struct {
		name string
		app  func(*testing.T) (*App, string)
	}{
		{name: "unknown key", app: newUnknownKeyApp},
		{name: "deleted key", app: newDeletedKeyApp},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, scope := test.app(t)
			if response := interceptBypass(t, app, scope, "mystery-model", test.name); response.Terminate {
				t.Fatalf("%s was terminated: %+v", test.name, response)
			}
		})
	}
}

// The scheduler resolves policy before observing candidates: unmanaged traffic
// never mutates the credential inventory and never takes over selection.
func TestUnmanagedSchedulerBypassesBeforeCandidateObservation(t *testing.T) {
	for _, test := range []struct {
		name string
		app  func(*testing.T) (*App, string)
	}{
		{name: "unmanaged", app: newUnmanagedApp},
		{name: "unknown key", app: newUnknownKeyApp},
		{name: "deleted key", app: newDeletedKeyApp},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, scope := test.app(t)
			raw, errHandle := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope,
				SchedulerAuthCandidate{ID: "file-codex", Provider: "codex", Attributes: map[string]string{"path": "/auth/codex.json"}},
				SchedulerAuthCandidate{ID: "config-codex", Provider: "codex", Attributes: map[string]string{"source": "config:codex[abc]"}},
			)))
			if errHandle != nil {
				t.Fatal(errHandle)
			}
			var response SchedulerPickResponse
			decodeResult(t, raw, &response)
			if response.Handled {
				t.Fatalf("%s scheduler handled selection: %+v", test.name, response)
			}
			if len(app.credentialInventory()) != 0 {
				t.Fatalf("%s scheduler observed candidates: %+v", test.name, app.credentialInventory())
			}
		})
	}
}

// Unmanaged usage is not accounted: no event, error, quota change, traffic-only
// key, or credential observation.
func TestUnmanagedUsageRecordsNothing(t *testing.T) {
	app, scope := newUnmanagedApp(t)
	records := []UsageRecord{
		{
			Provider: "openai", ExecutorType: "OpenAICompatExecutor", Model: flowModel, Alias: flowModel,
			APIKey: bypassKey, AuthIndex: "auth-u1", AuthType: "apikey", Source: "sk-upstream-0001",
			Generate: true, RequestedAt: app.store.Now(),
			Detail: UsageDetail{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
		},
		{
			Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: bypassKey,
			Generate: true, Failed: true, RequestedAt: app.store.Now(),
			Failure: UsageFailure{StatusCode: 502, Body: `{"error":{"type":"bad_gateway"}}`},
		},
		// An unknown key must not even gain a traffic-created identity.
		{
			Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: "sk-usage-unknown-1",
			Generate: true, RequestedAt: app.store.Now(),
			Detail: UsageDetail{InputTokens: 10, TotalTokens: 10},
		},
	}
	for _, record := range records {
		publishUsageRecord(t, app, record)
	}

	if entries := requestEventEntries(t, app); len(entries) != 0 {
		t.Fatalf("unmanaged usage recorded events: %+v", entries)
	}
	errors, err := app.store.RequestErrors(billing.RequestErrorQuery{})
	if err != nil || len(errors.Entries) != 0 {
		t.Fatalf("unmanaged usage recorded errors: %+v, %v", errors, err)
	}
	if views := app.store.KeyViews(); len(views) != 1 || views[0].Scope != scope || len(views[0].Windows) != 0 {
		t.Fatalf("unmanaged usage changed key inventory or quota: %+v", views)
	}
	if len(app.credentialInventory()) != 0 {
		t.Fatalf("unmanaged usage observed credentials: %+v", app.credentialInventory())
	}
}

// Current-state race semantics, documented for operators: admission and usage
// each resolve the policy at the time of their own callback.
func TestBindUnbindRaceFollowsCurrentState(t *testing.T) {
	t.Run("admitted managed, usage after unbind is dropped", func(t *testing.T) {
		app, scope := newManagedApp(t)
		if response := interceptBypass(t, app, scope, flowModel, "race-unbind"); response.Terminate {
			t.Fatalf("managed admission terminated: %+v", response)
		}
		if errUnbind := app.store.UnbindKey(scope); errUnbind != nil {
			t.Fatalf("UnbindKey error = %v", errUnbind)
		}
		publishUsageRecord(t, app, UsageRecord{
			Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: bypassKey,
			Generate: true, RequestedAt: app.store.Now(),
			Detail: UsageDetail{InputTokens: 10, TotalTokens: 10},
		})
		if entries := requestEventEntries(t, app); len(entries) != 0 {
			t.Fatalf("usage after unbind was recorded: %+v", entries)
		}
	})
	t.Run("admitted unmanaged, usage after binding is recorded", func(t *testing.T) {
		app, scope := newUnmanagedApp(t)
		if response := interceptBypass(t, app, scope, flowModel, "race-bind-usage"); response.Terminate {
			t.Fatalf("unmanaged admission terminated: %+v", response)
		}
		_, errCreate := app.store.CreatePlanWithBindings(billing.Plan{
			Name: "Race plan", Windows: []billing.QuotaWindow{{Name: "额度", AmountUSD: 100, PeriodSeconds: 86400}},
		}, []string{scope})
		if errCreate != nil {
			t.Fatalf("CreatePlanWithBindings error = %v", errCreate)
		}
		publishUsageRecord(t, app, UsageRecord{
			Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: bypassKey,
			Generate: true, RequestedAt: app.store.Now(),
			Detail: UsageDetail{InputTokens: 10, TotalTokens: 10},
		})
		if entries := requestEventEntries(t, app); len(entries) != 1 {
			t.Fatalf("usage after binding was not recorded: %+v", entries)
		}
	})
}

// Completion cleanup never depends on the current key policy: an admission
// made while managed releases its slot even after the key is unbound or the
// plugin is disabled in between.
func TestCompletionReleasesManagedStateAfterUnbind(t *testing.T) {
	app, scope := newManagedApp(t)
	if errSet := app.store.SetConcurrencyLimit(scope, 1); errSet != nil {
		t.Fatal(errSet)
	}
	if response := interceptBypass(t, app, scope, flowModel, "unbind-completion"); response.Terminate {
		t.Fatalf("managed admission terminated: %+v", response)
	}
	if errUnbind := app.store.UnbindKey(scope); errUnbind != nil {
		t.Fatal(errUnbind)
	}
	completeRequest(t, app, "unbind-completion")
	completeRequest(t, app, "unbind-completion")
	view, _ := app.store.KeyViewForScope(scope)
	if view.CurrentConcurrency != 0 {
		t.Fatalf("unbind stranded a concurrency slot: %+v", view)
	}
}

func TestCompletionReleasesSlotAfterPluginDisable(t *testing.T) {
	app, statePath := newAppWithPriceAndState(t, true)
	scope := manageKey(t, app, bypassKey)
	if errSet := app.store.SetConcurrencyLimit(scope, 1); errSet != nil {
		t.Fatal(errSet)
	}
	if response := interceptBypass(t, app, scope, flowModel, "disable-completion"); response.Terminate {
		t.Fatalf("managed admission terminated: %+v", response)
	}
	// Disabling keeps the same state file, so only the enabled flag changes.
	cfg := billing.DefaultConfig()
	cfg.Enabled = false
	cfg.StateFile = statePath
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if response := interceptBypass(t, app, scope, flowModel, "disable-after"); response.Terminate {
		t.Fatalf("disabled plugin terminated a request: %+v", response)
	}
	completeRequest(t, app, "disable-completion")
	if view, ok := app.store.KeyViewForScope(scope); !ok || view.CurrentConcurrency != 0 {
		t.Fatalf("disable stranded a concurrency slot: %+v (ok=%t)", view, ok)
	}
}

// A route log created while managed is still emitted when the key is unbound
// before completion; unmanaged admissions never create one.
func TestRouteLogOutlivesUnbind(t *testing.T) {
	app, scope := newManagedApp(t)
	if _, errCreate := app.store.CreateRoute(billing.Route{
		Name: "Watch", Rule: billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("auth-a")}},
	}, []string{scope}); errCreate != nil {
		t.Fatal(errCreate)
	}
	if response := interceptBypass(t, app, scope, flowModel, "unbind-route-log"); response.Terminate {
		t.Fatalf("managed admission terminated: %+v", response)
	}
	if errUnbind := app.store.UnbindKey(scope); errUnbind != nil {
		t.Fatal(errUnbind)
	}
	completeRequest(t, app, "unbind-route-log")
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Levels: []billing.PluginLogLevel{billing.PluginLogDebug}, Limit: 10})
	if err != nil || len(page.Entries) != 1 || !strings.HasPrefix(page.Entries[0].Message, "route ") {
		t.Fatalf("route log after unbind: %+v, %v", page.Entries, err)
	}
	if len(app.pending) != 0 {
		t.Fatalf("completion left pending route logs: %d", len(app.pending))
	}
}

// A repository whose snapshot loses its plans after validation models in-memory
// corruption. Admission must fail with a controlled 503 naming the missing
// plan, never degrade the key to an unmanaged pass-through.
type corruptingRepository struct {
	billing.Repository
}

func (r *corruptingRepository) Load(requestCutoff, pluginCutoff time.Time) (billing.Snapshot, error) {
	snapshot, err := r.Repository.Load(requestCutoff, pluginCutoff)
	if err == nil {
		snapshot.State.Plans = nil
	}
	return snapshot, err
}

func TestBrokenPlanBindingFailsAdmission(t *testing.T) {
	app, path := newAppWithPriceAndState(t, true)
	manageKey(t, app, bypassKey)
	app.Shutdown()

	reopened := newApp(billing.NewStore(func(statePath string) (billing.Repository, error) {
		repo, errOpen := openRepository(statePath)
		if errOpen != nil {
			return nil, errOpen
		}
		return &corruptingRepository{Repository: repo}, nil
	}, nil))
	t.Cleanup(reopened.Shutdown)
	if _, errHandle := reopened.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); errHandle != nil {
		t.Fatalf("plugin.register error = %v", errHandle)
	}

	response := interceptBypass(t, reopened, billing.CallerScope(bypassKey), flowModel, "broken-binding")
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response = %+v, want a terminating 503", response)
	}
	if !strings.Contains(string(response.ResponseBody), "subscription_configuration_error") {
		t.Fatalf("body = %s, want subscription_configuration_error", response.ResponseBody)
	}
	// The usage path stays managed for the broken binding: the event is still
	// recorded honestly instead of vanishing with the unmanaged bypass.
	publishUsageRecord(t, reopened, UsageRecord{
		Provider: "openai", Model: flowModel, Alias: flowModel, APIKey: bypassKey,
		Generate: true, RequestedAt: reopened.store.Now(),
		Detail: UsageDetail{InputTokens: 10, TotalTokens: 10},
	})
	if entries := requestEventEntries(t, reopened); len(entries) != 1 {
		t.Fatalf("broken binding usage events = %+v, want the managed record kept", entries)
	}
}
