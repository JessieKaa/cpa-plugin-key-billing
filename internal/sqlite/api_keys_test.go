package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestSavingKeysNeverDeletesExistingRows(t *testing.T) {
	database := openTestDB(t)
	state := billing.NewState()
	state.Keys["active"] = &billing.KeyState{Preview: "sk-tes…0001", InConfig: true}
	state.Keys["deleted"] = &billing.KeyState{Preview: "sk-tes…0002", DeletedAt: time.Unix(100, 0)}
	mustSave(t, database, state, billing.Changes{AllKeys: true})
	if _, err := database.db.Exec(`CREATE TRIGGER reject_key_deletion BEFORE DELETE ON api_keys
        BEGIN SELECT RAISE(ABORT, 'API Key records must be retained'); END`); err != nil {
		t.Fatal(err)
	}
	state.Keys["invalid"] = &billing.KeyState{}
	if err := database.Save(state, billing.Changes{AllKeys: true}); err == nil {
		t.Fatal("accepted a key without a preview")
	}
	delete(state.Keys, "invalid")
	delete(state.Keys, "deleted")
	state.Keys["active"].Label = "Updated"
	mustSave(t, database, state, billing.Changes{AllKeys: true})
	if err := database.Save(state, billing.Changes{Keys: []string{"deleted"}}); err == nil {
		t.Fatal("a missing key was accepted as a deletion")
	}
	mustSave(t, database, billing.NewState(), billing.Changes{AllKeys: true})
	keys := mustLoad(t, database).State.Keys
	if len(keys) != 2 || keys["active"].Label != "Updated" || keys["deleted"].Preview != "sk-tes…0002" || keys["deleted"].DeletedAt.IsZero() {
		t.Fatalf("saving keys deleted or replaced existing records: %+v", keys)
	}
}

// Fork compatibility: an upstream-shaped v17 database — including unbound
// traffic-created keys with usage history — loads unchanged, the fork can add
// managed data through the same schema, and the database still reports schema
// version 17 so upstream can reopen it.
func TestForkAndUpstreamShareSchemaVersion17(t *testing.T) {
	database := openTestDB(t)
	state := billing.NewState()
	state.Keys["unbound"] = &billing.KeyState{Preview: "*******"}
	state.Plans = []billing.Plan{{ID: "fork-plan", Name: "Fork", Windows: []billing.QuotaWindow{
		{ID: "w", Name: "额度", AmountUSD: 5, PeriodSeconds: 3600},
	}}}
	state.Keys["managed"] = &billing.KeyState{Preview: "sk-man…0001", InConfig: true, PlanID: "fork-plan"}
	mustSave(t, database, state, billing.Changes{AllKeys: true, Plans: true})
	if err := database.Save(billing.NewState(), billing.Changes{RequestErrorEvents: []billing.RequestErrorEvent{
		{Event: billing.RequestEvent{At: time.Unix(1, 0), Scope: "unbound", UpstreamModel: "gpt-5.5"}},
	}}); err != nil {
		t.Fatal(err)
	}

	var version int
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 17 {
		t.Fatalf("schema version = %d, want 17 (err=%v)", version, err)
	}
	reopened := mustLoad(t, database).State
	if key := reopened.Keys["unbound"]; key == nil || key.PlanID != "" {
		t.Fatalf("upstream unbound key lost: %+v", key)
	}
	if key := reopened.Keys["managed"]; key == nil || key.PlanID != "fork-plan" {
		t.Fatalf("fork managed key lost: %+v", key)
	}
	events, errEvents := database.RequestEvents(billing.RequestEventQuery{}, time.Time{})
	if errEvents != nil || events.Total != 1 || events.Entries[0].Scope != "unbound" {
		t.Fatalf("historical usage lost: %+v, %v", events, errEvents)
	}
}

// A stored scope that is not already canonical would be addressed by its
// normalized form at request time, so loading fails instead of turning a bound
// key into an unmanaged pass-through.
func TestLoadingRejectsNonCanonicalScope(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.db.Exec(insertKey, "UPPERCASE-SCOPE", "sk-tes…0001", "", true, 0, "", 0, `{}`, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Load(time.Time{}, time.Time{}); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("load error = %v, want a non-canonical scope failure", err)
	}
}

// Malformed plan data prevents plugin state from loading.
func TestLoadingRejectsMalformedPlanJSON(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.db.Exec(`INSERT INTO plans (position, id, name, windows_json)
		VALUES (0, 'p', 'P', 'not-json')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Load(time.Time{}, time.Time{}); err == nil || !strings.Contains(err.Error(), "subscription plan") {
		t.Fatalf("load error = %v, want a malformed plan failure", err)
	}
}

// A persisted quota cycle that no longer matches its plan window prevents
// plugin state from loading.
func TestLoadingRejectsInvalidQuotaCycle(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.db.Exec(`INSERT INTO plans (position, id, name, windows_json)
		VALUES (0, 'p', 'P', '[{"id":"w","name":"额度","period_seconds":3600,"amount_usd":10}]')`); err != nil {
		t.Fatal(err)
	}
	cycles := `{"missing":{"plan_id":"p","start_at":"2026-09-08T12:00:00Z","end_at":"2026-09-08T13:00:00Z","spent_usd":0}}`
	if _, err := database.db.Exec(insertKey, "dummy-scope", "sk-dum…0001", "", true, 0, "p", 0, cycles, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Load(time.Time{}, time.Time{}); err == nil || !strings.Contains(err.Error(), "Invalid quota cycle") {
		t.Fatalf("load error = %v, want an invalid cycle failure", err)
	}
}

// A key bound to a missing plan fails loading instead of being silently
// unbound: corrupt state stays a configuration error.
func TestLoadingFailsWhenABoundPlanRowIsMissing(t *testing.T) {
	database := openTestDB(t)
	state := billing.NewState()
	state.Plans = []billing.Plan{{ID: "gone", Name: "Gone", Windows: []billing.QuotaWindow{
		{ID: "w", Name: "额度", AmountUSD: 5, PeriodSeconds: 3600},
	}}}
	state.Keys["bound"] = &billing.KeyState{Preview: "sk-tes…0001", InConfig: true, PlanID: "gone"}
	mustSave(t, database, state, billing.Changes{AllKeys: true, Plans: true})
	if _, err := database.db.Exec(`DELETE FROM plans`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Load(time.Time{}, time.Time{}); err == nil ||
		!strings.Contains(err.Error(), "subscription plan bound to this API key does not exist") {
		t.Fatalf("load error = %v, want a missing-plan failure", err)
	}
}

func TestOldKeyPreviewRepairPreservesHistoryAndCanBeResolved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database := openDatabase(t, path)
	defer database.Close()
	const apiKey = "sk-dummy-legacy-0001"
	scope := billing.CallerScope(apiKey)
	state := billing.NewState()
	state.Plans = []billing.Plan{{ID: "plan", Windows: []billing.QuotaWindow{{ID: "default", Name: "额度", AmountUSD: 10, PeriodSeconds: 3600}}}}
	state.Keys[scope] = &billing.KeyState{
		Preview: billing.PreviewKey(apiKey), Label: "Legacy", PlanID: "plan",
		ConcurrencyLimit: 3, DeletedAt: time.Unix(100, 0), Cycles: map[string]billing.QuotaCycle{"default": {PlanID: "plan", StartAt: time.Unix(1, 0), EndAt: time.Unix(3601, 0), SpentUSD: 2}},
		RouteBindings: billing.RouteBindings{RouteRule: billing.RouteRule{Models: []string{"gpt-5.5"}}},
	}
	mustSave(t, database, state, billing.Changes{AllKeys: true, Plans: true,
		NormalRequestEvents: []billing.RequestEvent{requestEvent(scope, time.Now())},
	})
	if _, err := database.db.Exec("UPDATE api_keys SET preview = '' WHERE scope = ?", scope); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		key := mustLoad(t, database).State.Keys[scope]
		if key == nil || key.Preview != billing.UnknownKeyPreview || key.Label != "Legacy" ||
			key.PlanID != "plan" || key.ConcurrencyLimit != 3 || key.Cycles["default"].SpentUSD != 2 ||
			!key.DeletedAt.Equal(time.Unix(100, 0)) || len(key.RouteBindings.Models) != 1 {
			t.Fatalf("preview repair changed key state: %+v", key)
		}
	}
	var preview string
	if err := database.db.QueryRow("SELECT preview FROM api_keys WHERE scope = ?", scope).Scan(&preview); err != nil || preview != billing.UnknownKeyPreview {
		t.Fatalf("repair was not persisted: %q, %v", preview, err)
	}
	store := billing.NewStore(func(path string) (billing.Repository, error) { return Open(path) }, nil)
	defer store.Close()
	cfg := billing.DefaultConfig()
	cfg.StateFile = path
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(billing.UsageEvent{Scope: scope, KeyPreview: billing.PreviewKey(apiKey), UpstreamModel: "gpt-5.5"})
	key := mustLoad(t, database).State.Keys[scope]
	if key.Preview != billing.PreviewKey(apiKey) || key.DeletedAt.IsZero() {
		t.Fatalf("usage did not resolve the mask or resurrected the key: %+v", key)
	}
	view, err := store.RequestEvents(billing.RequestEventQuery{})
	if err != nil || len(view.Entries) != 2 || view.Entries[0].Preview != billing.PreviewKey(apiKey) || view.Entries[1].Preview != billing.PreviewKey(apiKey) {
		t.Fatalf("historical identity was lost: %+v, %v", view, err)
	}
}
