package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/purplestack/glucoguard/internal/app"
	"github.com/purplestack/glucoguard/internal/store"
)

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *app.Service
	st    store.Store
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, clock: time.Now().UTC().Truncate(time.Second)}
	// Set TEST_DATABASE_URL to run the whole suite against PostgreSQL.
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		pg, err := store.NewPostgres(context.Background(), dsn)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		t.Cleanup(pg.Close)
		h.st = pg
	} else {
		h.st = store.NewMemory()
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.svc = app.New(h.st, log)
	h.svc.Now = func() time.Time { return h.clock }
	h.srv = httptest.NewServer(NewServer(h.svc, DevHeaderAuth{}, log).Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) do(method, path, auth string, body any) (int, map[string]any, string) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if auth != "" {
		req.Header.Set("Authorization", "Dev "+auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func (h *harness) register(extra map[string]any) string {
	h.t.Helper()
	body := map[string]any{"display_name": "Asha", "age_group": "adult", "diabetes_type": "type1",
		"consent_given": true, "consent_version": app.ConsentVersion, "caregiver_phone": "+919876543210"}
	for k, v := range extra {
		body[k] = v
	}
	code, out, raw := h.do("POST", "/v1/patients", "", body)
	if code != 201 {
		h.t.Fatalf("register: %d %s", code, raw)
	}
	return out["id"].(string)
}

func (h *harness) reading(pid, clientID string, mg float64, ago time.Duration, symptoms ...string) (int, map[string]any) {
	h.t.Helper()
	if symptoms == nil {
		symptoms = []string{}
	}
	code, out, _ := h.do("POST", "/v1/glucose-readings", "patient:"+pid, map[string]any{
		"client_reading_id": clientID, "value": mg, "unit": "mg/dL", "source": "manual_meter",
		"measured_at": h.clock.Add(-ago).Format(time.RFC3339), "symptoms": symptoms,
	})
	return code, out
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func TestRegistrationRequiresConsent(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.do("POST", "/v1/patients", "", map[string]any{"display_name": "A", "age_group": "adult", "diabetes_type": "type2"})
	if code != 422 {
		t.Fatalf("want 422 without consent, got %d", code)
	}
	code, _, _ = h.do("POST", "/v1/patients", "", map[string]any{"display_name": "A", "age_group": "adult",
		"diabetes_type": "type2", "consent_given": true, "consent_version": app.ConsentVersion, "caregiver_phone": "12345"})
	if code != 422 {
		t.Fatalf("want 422 for bad phone, got %d", code)
	}
}

func TestLowGlucoseJourney(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	auth := "patient:" + pid

	// 1. Low reading, no plan yet: ADA thresholds still apply.
	code, out := h.reading(pid, "r1", 62, time.Minute)
	if code != 201 {
		t.Fatalf("submit: %d %v", code, out)
	}
	if got := dig(out, "evaluation", "decision", "action_code"); got != "TREAT_LOW" {
		t.Fatalf("action = %v", got)
	}
	if got := dig(out, "evaluation", "decision", "follow_up_policy", "recheck_after_minutes"); got != 15.0 {
		t.Fatalf("recheck = %v", got)
	}
	if dig(out, "message", "body") == nil || dig(out, "message", "review_status") != "draft_pending_clinician_review" {
		t.Fatalf("message missing or not marked draft: %v", out["message"])
	}
	evalID := dig(out, "evaluation", "id").(string)

	// 2. Retrying the same submission is idempotent.
	code, dup := h.reading(pid, "r1", 62, time.Minute)
	if code != 200 || dup["duplicate"] != true || dig(dup, "evaluation", "id") != evalID {
		t.Fatalf("idempotency broken: %d %v", code, dup)
	}

	// 3. Recheck after 15 minutes, still low: cycle 2.
	h.advance(15 * time.Minute)
	code, fu, raw := h.do("POST", "/v1/follow-ups", auth, map[string]any{
		"evaluation_id": evalID, "kind": "recheck",
		"recheck": map[string]any{"client_reading_id": "r2", "value": 64, "unit": "mg/dL", "measured_at": h.clock.Format(time.RFC3339)},
	})
	if code != 201 {
		t.Fatalf("follow-up: %d %s", code, raw)
	}
	if got := dig(fu, "new_evaluation", "evaluation", "decision", "low_treatment_cycle"); got != 2.0 {
		t.Fatalf("cycle = %v", got)
	}
	eval2 := dig(fu, "new_evaluation", "evaluation", "id").(string)

	// 4. Still low after the maximum cycles: escalate.
	h.advance(15 * time.Minute)
	_, fu, _ = h.do("POST", "/v1/follow-ups", auth, map[string]any{
		"evaluation_id": eval2, "kind": "recheck",
		"recheck": map[string]any{"client_reading_id": "r3", "value": 66, "unit": "mg/dL", "measured_at": h.clock.Format(time.RFC3339)},
	})
	d := dig(fu, "new_evaluation", "evaluation", "decision").(map[string]any)
	if d["action_code"] != "PERSISTENT_LOW_SEEK_HELP" || d["urgency"] != "urgent" {
		t.Fatalf("expected escalation, got %v", d)
	}
	if dig(d, "escalation", "notify_caregiver") != true {
		t.Fatalf("caregiver not notified: %v", d["escalation"])
	}

	// 5. Recovered.
	h.advance(15 * time.Minute)
	_, out = h.reading(pid, "r4", 95, 0)
	if got := dig(out, "evaluation", "decision", "action_code"); got != "SET_UP_CARE_PLAN" {
		t.Fatalf("in range without plan should prompt plan set-up, got %v", got)
	}

	// Audit trail records every rule execution.
	_, aud, _ := h.do("GET", "/v1/audit-events", auth, nil)
	n := 0
	for _, e := range aud["events"].([]any) {
		if e.(map[string]any)["action"] == "rule.executed" {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("want 4 rule executions audited, got %d", n)
	}
}

func TestSymptomUpdateEscalatesToEmergency(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	_, out := h.reading(pid, "r1", 58, time.Minute)
	evalID := dig(out, "evaluation", "id").(string)

	h.advance(30 * time.Minute) // the reading is now stale; emergency must still fire
	code, fu, raw := h.do("POST", "/v1/follow-ups", "patient:"+pid, map[string]any{
		"evaluation_id": evalID, "kind": "symptom_update", "symptoms": []string{"unable_to_swallow"},
	})
	if code != 201 {
		t.Fatalf("%d %s", code, raw)
	}
	d := dig(fu, "new_evaluation", "evaluation", "decision").(map[string]any)
	if d["action_code"] != "EMERGENCY_NO_ORAL" || dig(d, "escalation", "emergency_number") != "112" {
		t.Fatalf("expected emergency, got %v", d)
	}
	if d["data_status"] != "stale" {
		t.Fatalf("data_status = %v", d["data_status"])
	}
}

func TestReEvaluateShowsStaleness(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	_, out := h.reading(pid, "r1", 110, 0)
	rid := dig(out, "reading", "id").(string)
	h.advance(time.Hour)
	code, ev, _ := h.do("POST", "/v1/readings/"+rid+"/evaluate", "patient:"+pid, nil)
	if code != 200 || dig(ev, "evaluation", "decision", "action_code") != "CHECK_NOW_STALE_DATA" {
		t.Fatalf("expected stale: %d %v", code, ev)
	}
}

func TestCarePlanIsClinicianOnly(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	plan := map[string]any{"version": "v1", "low_mg_dl": 70, "severe_low_mg_dl": 54, "high_mg_dl": 180,
		"very_high_mg_dl": 300, "stale_after_minutes": 15, "low_recheck_after_minutes": 15,
		"high_recheck_after_minutes": 120, "max_low_treatment_cycles": 2, "approve": true}

	if code, _, _ := h.do("POST", "/v1/patients/"+pid+"/care-plans", "patient:"+pid, plan); code != 403 {
		t.Fatalf("patient must not create plans, got %d", code)
	}
	bad := map[string]any{}
	for k, v := range plan {
		bad[k] = v
	}
	bad["low_mg_dl"] = 50 // below severe low
	if code, _, _ := h.do("POST", "/v1/patients/"+pid+"/care-plans", "clinician:dr-mehta", bad); code != 422 {
		t.Fatalf("incoherent plan must be rejected, got %d", code)
	}
	code, out, raw := h.do("POST", "/v1/patients/"+pid+"/care-plans", "clinician:dr-mehta", plan)
	if code != 201 || out["approved_by"] != "dr-mehta" {
		t.Fatalf("create plan: %d %s", code, raw)
	}
	if code, _, _ := h.do("POST", "/v1/patients/"+pid+"/care-plans", "clinician:dr-mehta", plan); code != 409 {
		t.Fatalf("duplicate version must conflict, got %d", code)
	}

	_, cur, _ := h.do("GET", "/v1/action-plans/current", "patient:"+pid, nil)
	if dig(cur, "care_plan", "version") != "v1" {
		t.Fatalf("current plan = %v", cur)
	}
	_, r := h.reading(pid, "r1", 220, 0)
	if got := dig(r, "evaluation", "decision", "action_code"); got != "FOLLOW_HIGH_PLAN" {
		t.Fatalf("approved plan should drive high guidance, got %v", got)
	}
}

func TestPregnancyNeverGetsAdultPathway(t *testing.T) {
	h := newHarness(t)
	pid := h.register(map[string]any{"pregnant": true})
	_, out := h.reading(pid, "r1", 62, 0)
	if got := dig(out, "evaluation", "decision", "action_code"); got != "SPECIAL_PROTOCOL_CONTACT_CARE_TEAM" {
		t.Fatalf("got %v", got)
	}
}

func TestAuthAndIsolation(t *testing.T) {
	h := newHarness(t)
	a := h.register(nil)
	b := h.register(map[string]any{"display_name": "Bilal"})
	_, out := h.reading(a, "r1", 100, 0)
	rid := dig(out, "reading", "id").(string)

	if code, _, _ := h.do("GET", "/v1/glucose-readings", "", nil); code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/readings/"+rid+"/evaluate", "patient:"+b, nil); code != 404 {
		t.Fatalf("patient B must not reach patient A's reading, got %d", code)
	}
	_, list, _ := h.do("GET", "/v1/glucose-readings", "patient:"+b, nil)
	if n := len(list["readings"].([]any)); n != 0 {
		t.Fatalf("patient B sees %d readings", n)
	}
}

func TestInputValidation(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	auth := "patient:" + pid
	cases := []map[string]any{
		{"client_reading_id": "x", "value": 100, "unit": "mg", "measured_at": h.clock.Format(time.RFC3339)},
		{"client_reading_id": "x", "value": 100, "unit": "mg/dL"},
		{"client_reading_id": "x", "value": 100, "unit": "mg/dL", "measured_at": h.clock.Format(time.RFC3339), "symptoms": []string{"itchy"}},
		{"value": 100, "unit": "mg/dL", "measured_at": h.clock.Format(time.RFC3339)},
	}
	for i, c := range cases {
		if code, _, raw := h.do("POST", "/v1/glucose-readings", auth, c); code != 422 {
			t.Errorf("case %d: want 422, got %d %s", i, code, raw)
		}
	}
	if code, _, _ := h.do("POST", "/v1/glucose-readings", auth, map[string]any{"bogus": 1}); code != 400 {
		t.Errorf("unknown fields must be rejected, got %d", code)
	}
	// mmol/L and meter LO are accepted.
	code, out, _ := h.do("POST", "/v1/glucose-readings", auth, map[string]any{"client_reading_id": "m", "value": 3.2,
		"unit": "mmol/L", "measured_at": h.clock.Format(time.RFC3339)})
	if code != 201 || dig(out, "reading", "value_mg_dl") != 57.7 {
		t.Fatalf("mmol: %d %v", code, out["reading"])
	}
	code, out, _ = h.do("POST", "/v1/glucose-readings", auth, map[string]any{"client_reading_id": "lo", "meter_flag": "LO",
		"unit": "mg/dL", "measured_at": h.clock.Format(time.RFC3339)})
	if code != 201 || dig(out, "evaluation", "decision", "action_code") != "TREAT_SEVERE_LOW" {
		t.Fatalf("LO: %d %v", code, out)
	}
}

func TestReportJSONAndCSV(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	for i, v := range []float64{50, 65, 110, 140, 260} {
		h.reading(pid, "r"+string(rune('a'+i)), v, time.Duration(5-i)*time.Hour)
	}
	_, rep, _ := h.do("GET", "/v1/reports/glucose", "patient:"+pid, nil)
	st := rep["stats"].(map[string]any)
	if st["readings"] != 5.0 || st["readings_below_low_pct"] != 40.0 || st["readings_below_severe_low_pct"] != 20.0 {
		t.Fatalf("stats = %v", st)
	}
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/reports/glucose?format=csv", nil)
	req.Header.Set("Authorization", "Dev patient:"+pid)
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if lines := strings.Count(string(body), "\n"); lines != 6 {
		t.Fatalf("csv lines = %d\n%s", lines, body)
	}
	// Clinician route.
	if code, _, _ := h.do("GET", "/v1/patients/"+pid+"/reports/glucose", "clinician:dr-mehta", nil); code != 200 {
		t.Fatalf("clinician report: %d", code)
	}
}

// flakyNotifier counts deliveries for one patient only, so the test is not
// affected by other tests' reminders in a shared database.
type flakyNotifier struct {
	patient string
	fail    bool
	calls   int
}

func (f *flakyNotifier) NotifyRecheck(_ context.Context, r store.Reminder) error {
	if r.PatientID != f.patient {
		return nil
	}
	f.calls++
	if f.fail {
		return errors.New("push provider down")
	}
	return nil
}

func TestReminderDeliveryAndRetry(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	h.reading(pid, "r1", 62, 0)

	n := &flakyNotifier{patient: pid, fail: true}
	w := &app.ReminderWorker{Svc: h.svc, Notifier: n, Lease: time.Minute, MaxAttempts: 3}
	w.Tick(context.Background())
	if n.calls != 0 {
		t.Fatal("reminder fired before due")
	}
	for i := 0; i < 10; i++ {
		h.advance(16 * time.Minute)
		w.Tick(context.Background())
	}
	if n.calls != 3 {
		t.Fatalf("want 3 attempts then give up, got %d", n.calls)
	}
	_, aud, _ := h.do("GET", "/v1/audit-events", "patient:"+pid, nil)
	if !strings.Contains(mustJSON(aud), "reminder.delivery_exhausted") {
		t.Fatal("exhausted delivery not audited")
	}

	// A new reading supersedes the old reminder; a working notifier delivers once.
	h.reading(pid, "r2", 60, 0)
	ok := &flakyNotifier{patient: pid}
	w.Notifier = ok
	h.advance(16 * time.Minute)
	w.Tick(context.Background())
	w.Tick(context.Background())
	if ok.calls != 1 {
		t.Fatalf("want exactly one delivery, got %d", ok.calls)
	}
}

func TestAccountDeletion(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	h.reading(pid, "r1", 100, 0)
	if code, _, _ := h.do("DELETE", "/v1/patients/me", "patient:"+pid, nil); code != 422 {
		t.Fatalf("deletion needs confirmation, got %d", code)
	}
	if code, _, _ := h.do("DELETE", "/v1/patients/me?confirm=DELETE", "patient:"+pid, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/patients/me", "patient:"+pid, nil); code != 404 {
		t.Fatalf("patient still exists: %d", code)
	}
	if rs, _ := h.st.ListReadings(context.Background(), pid, store.TimeRange{}); len(rs) != 0 {
		t.Fatal("readings not erased")
	}
	if evs, _ := h.st.ListAudit(context.Background(), "", store.TimeRange{}); len(evs) == 0 {
		t.Fatal("de-identified audit trail should be kept")
	}
}

func TestDeviceSyncNotImplemented(t *testing.T) {
	h := newHarness(t)
	pid := h.register(nil)
	if code, _, _ := h.do("POST", "/v1/devices/libre/sync", "patient:"+pid, nil); code != 501 {
		t.Fatalf("got %d", code)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
