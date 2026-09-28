package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

const (
	defaultEventPageSize = 50
	maxEventPageSize     = 1000
)

func sourceFilterToken(source string) string {
	digest := sha256.Sum256([]byte("filter:v1\x00source\x00" + source))
	return hex.EncodeToString(digest[:])
}

func validSourceFilterToken(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func resolveSourceFilter(token string, sources []string) (string, bool) {
	for _, source := range sources {
		if sourceFilterToken(source) == token {
			return source, true
		}
	}
	return "", false
}

func sourceFilterOptions(sources []string) []billing.RequestSourceOption {
	options := make([]billing.RequestSourceOption, 0, len(sources))
	for _, source := range sources {
		options = append(options, billing.RequestSourceOption{
			Value: sourceFilterToken(source),
			Label: source,
		})
	}
	return options
}

// Admin handlers serve only management-authenticated flows. There is no
// downstream API-key access path in this fork.
func (a *App) listAdminRequestEvents(req ManagementRequest) ManagementResponse {
	query := billing.RequestEventQuery{
		Model:  strings.TrimSpace(req.Query.Get("model")),
		Source: strings.TrimSpace(req.Query.Get("source")), Executor: strings.TrimSpace(req.Query.Get("executor")),
		Provider: strings.TrimSpace(req.Query.Get("provider")),
		Limit:    defaultEventPageSize,
	}
	query.KeyScope = strings.TrimSpace(req.Query.Get("api_key"))
	switch raw := strings.TrimSpace(req.Query.Get("failed")); raw {
	case "":
	case "false", "true":
		failed := raw == "true"
		query.Failed = &failed
	default:
		return JSONError(http.StatusBadRequest, "invalid", "failed must be true or false")
	}
	if errQuery := requestPageParams(req.Query, &query.Offset, &query.Limit, &query.From, &query.To, &query.SnapshotID); errQuery != nil {
		return errorResponse(errQuery)
	}
	if query.Source != "" {
		if !validSourceFilterToken(query.Source) {
			return JSONError(http.StatusBadRequest, "invalid_filter", "Invalid source filter; refresh the page")
		}
		probe, err := a.store.RequestEvents(billing.RequestEventQuery{
			From: query.From, To: query.To, SnapshotID: query.SnapshotID, IncludeFilters: true, Limit: 1,
		})
		if err != nil {
			return errorResponse(err)
		}
		query.SnapshotID = &probe.SnapshotID
		if probe.Filters == nil {
			return JSONError(http.StatusBadRequest, "expired_filter", "Source filter expired; refresh the page")
		}
		var found bool
		query.Source, found = resolveSourceFilter(query.Source, probe.Filters.Sources)
		if !found {
			return JSONError(http.StatusBadRequest, "expired_filter", "Source filter expired; refresh the page")
		}
	}
	query.IncludeFilters = query.Offset == 0
	view, err := a.store.RequestEvents(query)
	if err != nil {
		return errorResponse(err)
	}
	if view.Filters != nil {
		view.Filters.SourceOptions = sourceFilterOptions(view.Filters.Sources)
	}
	return JSONResponse(http.StatusOK, view)
}

func (a *App) listAdminRequestErrors(req ManagementRequest) ManagementResponse {
	query := billing.RequestErrorQuery{
		Model:  strings.TrimSpace(req.Query.Get("model")),
		Source: strings.TrimSpace(req.Query.Get("source")), Executor: strings.TrimSpace(req.Query.Get("executor")),
		Provider: strings.TrimSpace(req.Query.Get("provider")), ErrorType: strings.TrimSpace(req.Query.Get("error_type")),
		ErrorTypeEmpty: req.Query.Get("error_type_empty") == "true",
		Limit:          defaultEventPageSize,
	}
	if raw := strings.TrimSpace(req.Query.Get("status_code")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 100 || value > 599 {
			return JSONError(http.StatusBadRequest, "invalid", "HTTP status code must be an integer from 100 to 599")
		}
		query.StatusCode = value
	}
	query.KeyScope = strings.TrimSpace(req.Query.Get("api_key"))
	if err := requestPageParams(req.Query, &query.Offset, &query.Limit, &query.From, &query.To, &query.SnapshotID); err != nil {
		return errorResponse(err)
	}
	if query.Source != "" {
		if !validSourceFilterToken(query.Source) {
			return JSONError(http.StatusBadRequest, "invalid_filter", "Invalid source filter; refresh the page")
		}
		probe, err := a.store.RequestErrors(billing.RequestErrorQuery{
			From: query.From, To: query.To, SnapshotID: query.SnapshotID, IncludeFilters: true, Limit: 1,
		})
		if err != nil {
			return errorResponse(err)
		}
		query.SnapshotID = &probe.SnapshotID
		if probe.Filters == nil {
			return JSONError(http.StatusBadRequest, "expired_filter", "Source filter expired; refresh the page")
		}
		var found bool
		query.Source, found = resolveSourceFilter(query.Source, probe.Filters.Sources)
		if !found {
			return JSONError(http.StatusBadRequest, "expired_filter", "Source filter expired; refresh the page")
		}
	}
	query.IncludeFilters = query.Offset == 0
	view, err := a.store.RequestErrors(query)
	if err != nil {
		return errorResponse(err)
	}
	if view.Filters != nil {
		view.Filters.SourceOptions = sourceFilterOptions(view.Filters.Sources)
	}
	return JSONResponse(http.StatusOK, view)
}

func (a *App) adminAnalysis(req ManagementRequest) ManagementResponse {
	query := billing.RequestEventQuery{KeyScope: strings.TrimSpace(req.Query.Get("api_key"))}
	if err := timeParam(req.Query, "from", &query.From); err != nil {
		return errorResponse(err)
	}
	if err := timeParam(req.Query, "to", &query.To); err != nil {
		return errorResponse(err)
	}
	if name := strings.TrimSpace(req.Query.Get("timezone")); name != "" {
		location, err := time.LoadLocation(name)
		if err != nil {
			return errorResponse(&billing.Error{
				Kind: billing.KindInvalid, Msg: "Timezone must be a valid IANA identifier",
			})
		}
		query.Timezone = location
	}
	view, err := a.store.Analysis(query)
	if err != nil {
		return errorResponse(err)
	}
	if query.KeyScope != "" {
		view.UsageDistribution.APIKeys = []billing.AnalysisComposition{}
	}
	return JSONResponse(http.StatusOK, view)
}

func requestPageParams(values url.Values, offset, limit *int, from, to *time.Time, snapshot **int64) error {
	if raw := strings.TrimSpace(values.Get("snapshot_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			return &billing.Error{Kind: billing.KindInvalid, Msg: "snapshot_id must be a non-negative integer"}
		}
		*snapshot = &id
	}
	if errOffset := countParam(values, "offset", offset); errOffset != nil {
		return errOffset
	}
	if errLimit := countParam(values, "limit", limit); errLimit != nil {
		return errLimit
	}
	if errFrom := timeParam(values, "from", from); errFrom != nil {
		return errFrom
	}
	if errTo := timeParam(values, "to", to); errTo != nil {
		return errTo
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(*to) {
		return &billing.Error{Kind: billing.KindInvalid, Msg: "Start time must be earlier than end time"}
	}
	if *limit < 1 || *limit > maxEventPageSize {
		return &billing.Error{Kind: billing.KindInvalid, Msg: "Limit must be an integer from 1 to 1000"}
	}
	return nil
}

func timeParam(query url.Values, name string, target *time.Time) error {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return nil
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		detail := messages.New("%s must be an RFC3339 timestamp", name)
		return &billing.Error{Kind: billing.KindInvalid, Msg: detail.Text, Detail: detail}
	}
	*target = parsed
	return nil
}

func countParam(query url.Values, name string, target *int) error {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return nil
	}
	parsed, errParse := strconv.Atoi(raw)
	if errParse != nil || parsed < 0 {
		detail := messages.New("%s must be a non-negative integer", name)
		return &billing.Error{Kind: billing.KindInvalid, Msg: detail.Text, Detail: detail}
	}
	*target = parsed
	return nil
}

func (a *App) eventKeys(req ManagementRequest) ManagementResponse {
	var from, to time.Time
	if err := timeParam(req.Query, "from", &from); err != nil {
		return errorResponse(err)
	}
	if err := timeParam(req.Query, "to", &to); err != nil {
		return errorResponse(err)
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return JSONError(http.StatusBadRequest, "invalid", "Start time must be earlier than end time")
	}
	keys, err := a.store.EventKeys(from, to)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, keys)
}
