package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestRegisterDeclaresExpectedCapabilities(t *testing.T) {
	app := newConfiguredApp(t)
	raw, errHandle := app.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{
		ConfigYAML: testConfigYAML(t, true),
	}))
	if errHandle != nil {
		t.Fatalf("plugin.reconfigure error = %v", errHandle)
	}
	var registration Registration
	decodeResult(t, raw, &registration)

	// The host refuses to load a plugin declaring more than it implements.
	if registration.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", registration.SchemaVersion, SchemaVersion)
	}
	caps := registration.Capabilities
	if !caps.RequestInterceptor || !caps.RequestLifecyclePlugin || !caps.UsagePlugin || !caps.ManagementAPI || !caps.Scheduler {
		t.Fatalf("capabilities = %+v, want every hook billing depends on", caps)
	}
	if registration.Metadata.Name != PluginName || registration.Metadata.Version != Version {
		t.Fatalf("metadata = %+v", registration.Metadata)
	}
	if len(registration.Metadata.ConfigFields) == 0 {
		t.Fatal("ConfigFields is empty, the panel needs them to render the config form")
	}
	fields := make(map[string]ConfigField, len(registration.Metadata.ConfigFields))
	for _, field := range registration.Metadata.ConfigFields {
		fields[field.Name] = field
	}
	if fields["state_file"].Type != "string" || fields["debug"].Type != "boolean" ||
		fields["codex_fast_mode_billing"].Type != "boolean" || fields["mask_api_key_view_emails"].Type != "boolean" ||
		fields["allow_api_key_quota_reset"].Type != "boolean" || fields["enabled"].Name != "" {
		t.Fatalf("ConfigFields = %+v", registration.Metadata.ConfigFields)
	}
}

func TestUnknownMethodReturnsErrorEnvelopeNotAnError(t *testing.T) {
	app := newConfiguredApp(t)
	raw, errHandle := app.HandleMethod("does.not.exist", nil)
	if errHandle != nil {
		t.Fatalf("unknown method returned a transport error = %v, want an error envelope", errHandle)
	}
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %+v, want an unknown_method error", envelope)
	}
}

func TestConfigureReportsReferencePricePreloadFailure(t *testing.T) {
	requests := 0
	app := newApp(billing.NewStore(openRepository, func(context.Context) ([]byte, error) {
		requests++
		return nil, errors.New("Download reference prices: HTTP 503")
	}))
	t.Cleanup(app.Shutdown)
	raw, errHandle := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: testConfigYAML(t, true),
	}))
	if errHandle != nil {
		t.Fatalf("plugin.register error = %v", errHandle)
	}
	decodeResult(t, raw, nil)
	if requests != 1 {
		t.Fatalf("reference price preload requests = %d, want 1", requests)
	}
	page, errEvents := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 500})
	if errEvents != nil {
		t.Fatal(errEvents)
	}
	events := page.Entries
	if len(events) == 0 || events[0].Level != billing.PluginLogError ||
		!strings.Contains(events[0].Message, "Failed to sync models.dev reference prices") {
		t.Fatalf("events = %+v, want the preload failure", events)
	}
}

func TestManagementRegistrationExposesOnlyCurrentEndpoints(t *testing.T) {
	registration := managementRegistration()
	wantRoutes := map[string]bool{}
	for _, value := range []string{
		"GET /keys", "GET /plans", "GET /routes", "GET /credentials", "GET /prices", "GET /prices/reference",
		"POST /prices/reference/refresh", "PUT /prices", "DELETE /prices", "GET /prices/reference/status",
		"POST /plans", "PATCH /plans", "DELETE /plans",
		"POST /routes", "PATCH /routes", "DELETE /routes", "PUT /keys/routes",
		"POST /keys/bind", "POST /keys/unbind", "POST /keys/reset",
		"POST /keys/label", "POST /keys/concurrency", "POST /keys/sync",
		"POST /credentials/sync",
		"GET /analysis", "GET /events", "GET /events/keys", "GET /errors",
		"GET /plugin-logs", "DELETE /plugin-logs", "GET /auth-files", "GET /auth-files/quota",
		"POST /auth-files/quota/reset",
	} {
		wantRoutes[value] = false
	}
	if len(registration.Routes) != len(wantRoutes) {
		t.Fatalf("routes = %d, want %d: %+v", len(registration.Routes), len(wantRoutes), registration.Routes)
	}
	for _, route := range registration.Routes {
		if !strings.HasPrefix(route.Path, managementBase+"/") || strings.ContainsAny(route.Path, ":*") {
			t.Fatalf("invalid management route: %+v", route)
		}
		key := route.Method + " " + strings.TrimPrefix(route.Path, managementBase)
		if _, ok := wantRoutes[key]; !ok {
			t.Fatalf("unexpected management route %q", key)
		}
		if wantRoutes[key] {
			t.Fatalf("duplicate management route %q", key)
		}
		wantRoutes[key] = true
	}

	// Registration guard: the fork registers zero Resource routes. Every
	// dynamic operation is a Management route; adding a Resource registration
	// or menu entry here must fail the build pipeline.
	if len(registration.Resources) != 0 {
		t.Fatalf("resources = %+v, want none", registration.Resources)
	}
}

// The former API-key resource paths no longer exist: every request under the
// plugin's resource prefix receives 404, with or without a downstream key.
func TestFormerResourceRoutesReturnNotFound(t *testing.T) {
	app := newConfiguredApp(t)
	const formerResourceBase = "/v0/resource/plugins/" + PluginID
	paths := []string{"/ui", "/profile", "/subscription", "/routing", "/prices", "/analysis", "/events", "/errors",
		"/auth-files", "/auth-files/quota", "/auth-files/quota/reset", "/"}
	hostCalls := 0
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		hostCalls++
		return nil, nil
	})
	for _, path := range paths {
		for _, withKey := range []bool{false, true} {
			req := ManagementRequest{Method: http.MethodGet, Path: formerResourceBase + path}
			if withKey {
				req.Headers = http.Header{"Authorization": {"Bearer sk-former-resource-key"}}
			}
			raw, errHandle := app.handleManagement(mustMarshal(t, req))
			if errHandle != nil {
				t.Fatalf("handleManagement(%q): %v", path, errHandle)
			}
			var response ManagementResponse
			decodeResult(t, raw, &response)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s (key=%t) status = %d, want 404", path, withKey, response.StatusCode)
			}
			if withKey && strings.Contains(string(response.Body), "sk-former-resource-key") {
				t.Fatalf("GET %s leaked the API key: %s", path, response.Body)
			}
		}
	}
	if hostCalls != 0 {
		t.Fatalf("resource requests reached privileged host callbacks: %d", hostCalls)
	}
}

// Deprecated account-portal YAML fields still parse, but enable no route,
// UI control, masking, or reset permission.
func TestDeprecatedAccountFieldsEnableNothing(t *testing.T) {
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	config := "enabled: true\nmask_api_key_view_emails: true\nallow_api_key_quota_reset: true\n" +
		"state_file: \"" + filepath.Join(t.TempDir(), "state.db") + "\"\n"
	raw, errHandle := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(config)}))
	if errHandle != nil {
		t.Fatalf("plugin.register with deprecated fields error = %v", errHandle)
	}
	decodeResult(t, raw, nil)
	if resources := managementRegistration().Resources; len(resources) != 0 {
		t.Fatalf("deprecated fields registered resources: %+v", resources)
	}
	const formerResourceBase = "/v0/resource/plugins/" + PluginID
	for _, path := range []string{"/ui", "/profile", "/auth-files/quota/reset"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			raw, errHandle := app.handleManagement(mustMarshal(t, ManagementRequest{
				Method: method, Path: formerResourceBase + path,
				Headers: http.Header{"Authorization": {"Bearer sk-deprecated-field-key"}},
			}))
			if errHandle != nil {
				t.Fatalf("handleManagement(%s %q): %v", method, path, errHandle)
			}
			var response ManagementResponse
			decodeResult(t, raw, &response)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404", method, path, response.StatusCode)
			}
		}
	}
}

// Every management response, success or error, carries no-store headers.
func TestManagementResponsesCarryNoStoreHeaders(t *testing.T) {
	app := newConfiguredApp(t)
	requests := []ManagementRequest{
		{Method: http.MethodGet, Path: managementBase + routeKeys},
		{Method: http.MethodGet, Path: managementBase + "/missing"},
		{Method: http.MethodPost, Path: managementBase + routeKeysLabel, Body: []byte(`{"scope":""}`)},
		{Method: http.MethodGet, Path: managementBase + routeEvents},
		{Method: http.MethodGet, Path: managementBase + routeEvents, Query: url.Values{"failed": {"unknown"}}},
	}
	for _, req := range requests {
		raw, errHandle := app.handleManagement(mustMarshal(t, req))
		if errHandle != nil {
			t.Fatalf("handleManagement(%s %s): %v", req.Method, req.Path, errHandle)
		}
		var response ManagementResponse
		decodeResult(t, raw, &response)
		if got := response.Headers.Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s %s Cache-Control = %q", req.Method, req.Path, got)
		}
		for header, want := range map[string]string{"Pragma": "no-cache", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
			if got := response.Headers.Get(header); got != want {
				t.Fatalf("%s %s %s = %q, want %q", req.Method, req.Path, header, got, want)
			}
		}
	}
}

func TestManagementRejectsLookalikeRoutePrefixes(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{managementBase + "-other/keys", "/v0/resource/plugins/" + PluginID + "-other/ui"} {
		raw, errHandle := app.handleManagement(mustMarshal(t, ManagementRequest{
			Method: http.MethodGet,
			Path:   path,
		}))
		if errHandle != nil {
			t.Fatalf("handleManagement(%q): %v", path, errHandle)
		}
		var envelope Envelope
		if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		var response ManagementResponse
		if errUnmarshal := json.Unmarshal(envelope.Result, &response); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("handleManagement(%q) status = %d", path, response.StatusCode)
		}
	}
}

func TestHandleMethodRecoversFromPanic(t *testing.T) {
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	app.store = nil
	_, errHandle := app.HandleMethod(MethodManagementHandle, mustMarshal(t, ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBase + routeKeys,
	}))
	if errHandle == nil {
		t.Fatal("a panicking handler returned no error")
	}
}
