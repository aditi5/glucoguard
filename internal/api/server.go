// Package api exposes the REST API. Handlers are thin: they authenticate,
// decode, call app.Service, and map errors. The backend, not the client, owns
// the authoritative safety outcome.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/purplestack/glucoguard/internal/app"
	"github.com/purplestack/glucoguard/internal/clinical"
	"github.com/purplestack/glucoguard/internal/store"
)

type Server struct {
	Svc     *app.Service
	Auth    Authenticator
	Log     *slog.Logger
	limiter *rateLimiter
}

func NewServer(svc *app.Service, auth Authenticator, log *slog.Logger) *Server {
	return &Server{Svc: svc, Auth: auth, Log: log, limiter: newRateLimiter(120, 40)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/meta", s.meta)
	mux.HandleFunc("GET /v1/messages", s.messages)

	mux.HandleFunc("POST /v1/patients", s.registerPatient)
	mux.HandleFunc("GET /v1/patients/me", s.requireAuth("patient", s.getMe))
	mux.HandleFunc("PUT /v1/patients/me", s.requireAuth("patient", s.updateMe))
	mux.HandleFunc("DELETE /v1/patients/me", s.requireAuth("patient", s.deleteMe))

	mux.HandleFunc("POST /v1/glucose-readings", s.requireAuth("patient", s.submitReading))
	mux.HandleFunc("GET /v1/glucose-readings", s.requireAuth("patient", s.listReadings))
	mux.HandleFunc("POST /v1/readings/{id}/evaluate", s.requireAuth("patient", s.evaluateReading))
	mux.HandleFunc("GET /v1/action-plans/current", s.requireAuth("patient", s.currentPlan))
	mux.HandleFunc("POST /v1/follow-ups", s.requireAuth("patient", s.followUp))
	mux.HandleFunc("POST /v1/devices/{provider}/sync", s.requireAuth("patient", s.deviceSync))
	mux.HandleFunc("GET /v1/reports/glucose", s.requireAuth("patient", s.report))
	mux.HandleFunc("GET /v1/audit-events", s.requireAuth("patient", s.auditEvents))

	// Clinician endpoints. TODO(before pilot): enforce care-team membership,
	// so a clinician can only act on their own patients.
	mux.HandleFunc("POST /v1/patients/{id}/care-plans", s.requireAuth("clinician", s.createCarePlan))
	mux.HandleFunc("GET /v1/patients/{id}/reports/glucose", s.requireAuth("clinician", s.report))
	return s.common(mux)
}

func actorOf(r *http.Request) app.Actor {
	p, _ := principalFrom(r.Context())
	return app.Actor{ID: p.ID, Role: p.Role}
}

// patientID is the caller's own ID for patients, or the {id} path value for
// clinician routes.
func patientID(r *http.Request) string {
	p, _ := principalFrom(r.Context())
	if p.Role == "clinician" {
		return r.PathValue("id")
	}
	return p.ID
}

// ---- public -----------------------------------------------------------------

func (s *Server) meta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"rule_set_version":             clinical.RuleSetVersion,
		"rule_set_clinically_approved": clinical.RuleSetClinicallyApproved,
		"messages_all_approved":        clinical.AllMessagesApproved(),
		"emergency_number":             clinical.EmergencyNumberIN,
		"consent_version":              app.ConsentVersion,
		"supported_units":              []clinical.Unit{clinical.UnitMgDL, clinical.UnitMmolL},
	})
}

func (s *Server) messages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"rule_set_version": clinical.RuleSetVersion, "messages": clinical.Catalog})
}

func (s *Server) registerPatient(w http.ResponseWriter, r *http.Request) {
	var in app.PatientInput
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Svc.RegisterPatient(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ---- patient ----------------------------------------------------------------

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p, err := s.Svc.Store.GetPatient(r.Context(), patientID(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var in app.PatientInput
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Svc.UpdatePatient(r.Context(), actorOf(r), patientID(r), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("confirm") != "DELETE" {
		writeError(w, 422, "confirmation_required", "add ?confirm=DELETE; this permanently erases all your data")
		return
	}
	if err := s.Svc.DeletePatient(r.Context(), actorOf(r), patientID(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) submitReading(w http.ResponseWriter, r *http.Request) {
	var in app.ReadingInput
	if !decode(w, r, &in) {
		return
	}
	if in.ClientReadingID == "" {
		in.ClientReadingID = r.Header.Get("Idempotency-Key")
	}
	res, err := s.Svc.SubmitReading(r.Context(), actorOf(r), patientID(r), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (s *Server) listReadings(w http.ResponseWriter, r *http.Request) {
	tr, err := parseRange(r)
	if err != nil {
		writeError(w, 422, "invalid", err.Error())
		return
	}
	if tr.Limit == 0 || tr.Limit > 1000 {
		tr.Limit = 200
	}
	rs, err := s.Svc.Store.ListReadings(r.Context(), patientID(r), tr)
	if err != nil {
		s.fail(w, err)
		return
	}
	if rs == nil {
		rs = []store.Reading{}
	}
	writeJSON(w, 200, map[string]any{"readings": rs})
}

func (s *Server) evaluateReading(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Symptoms []clinical.Symptom `json:"symptoms"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	res, err := s.Svc.EvaluateReading(r.Context(), actorOf(r), patientID(r), r.PathValue("id"), in.Symptoms)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) currentPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.Svc.CurrentPlan(r.Context(), patientID(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := map[string]any{"rule_set_version": clinical.RuleSetVersion, "care_plan": plan}
	if plan == nil {
		d := clinical.DefaultUnapprovedPlan()
		resp["care_plan"] = nil
		resp["using_defaults"] = true
		resp["defaults"] = map[string]any{"low_mg_dl": d.LowMgDL, "severe_low_mg_dl": d.SevereLowMgDL,
			"note": "No clinician-approved plan. Low thresholds follow ADA hypoglycemia definitions; high readings route to your care team."}
	}
	writeJSON(w, 200, resp)
}

func (s *Server) followUp(w http.ResponseWriter, r *http.Request) {
	var in app.FollowUpInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.Svc.RecordFollowUp(r.Context(), actorOf(r), patientID(r), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) deviceSync(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		fmt.Sprintf("device provider %q is not integrated yet; submit readings via POST /v1/glucose-readings", r.PathValue("provider")))
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	tr, err := parseRange(r)
	if err != nil {
		writeError(w, 422, "invalid", err.Error())
		return
	}
	rep, err := s.Svc.GlucoseReport(r.Context(), actorOf(r), patientID(r), tr.From, tr.To)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="glucose-report.csv"`)
		if err := rep.WriteCSV(w); err != nil {
			s.Log.Error("csv", "err", err)
		}
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) auditEvents(w http.ResponseWriter, r *http.Request) {
	tr, err := parseRange(r)
	if err != nil {
		writeError(w, 422, "invalid", err.Error())
		return
	}
	if tr.Limit == 0 || tr.Limit > 500 {
		tr.Limit = 100
	}
	evs, err := s.Svc.Store.ListAudit(r.Context(), patientID(r), tr)
	if err != nil {
		s.fail(w, err)
		return
	}
	if evs == nil {
		evs = []store.AuditEvent{}
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

// ---- clinician ----------------------------------------------------------------

func (s *Server) createCarePlan(w http.ResponseWriter, r *http.Request) {
	var in app.CarePlanInput
	if !decode(w, r, &in) {
		return
	}
	plan, err := s.Svc.CreateCarePlan(r.Context(), actorOf(r), r.PathValue("id"), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}

// ---- helpers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "invalid JSON body: "+err.Error())
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_json", "body must contain a single JSON object")
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ve *app.ValidationError
	var ef *app.EvaluationFailure
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "invalid", ve.Msg)
	case errors.As(err, &ef):
		// Never leave the patient without guidance.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":     map[string]string{"code": "evaluation_unavailable", "message": "the reading could not be evaluated"},
			"fail_safe": app.FailSafe,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, app.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "forbidden")
	default:
		s.Log.Error("internal error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func parseRange(r *http.Request) (store.TimeRange, error) {
	q := r.URL.Query()
	var tr store.TimeRange
	var err error
	if v := q.Get("from"); v != "" {
		if tr.From, err = time.Parse(time.RFC3339, v); err != nil {
			return tr, fmt.Errorf("from must be RFC3339")
		}
	}
	if v := q.Get("to"); v != "" {
		if tr.To, err = time.Parse(time.RFC3339, v); err != nil {
			return tr, fmt.Errorf("to must be RFC3339")
		}
	}
	if v := q.Get("limit"); v != "" {
		if tr.Limit, err = strconv.Atoi(v); err != nil || tr.Limit < 0 {
			return tr, fmt.Errorf("limit must be a non-negative integer")
		}
	}
	return tr, nil
}
