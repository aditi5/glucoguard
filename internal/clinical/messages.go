package clinical

// ReviewStatus tracks clinician sign-off for each patient-facing message.
type ReviewStatus string

const (
	ReviewDraft    ReviewStatus = "draft_pending_clinician_review"
	ReviewApproved ReviewStatus = "approved"
)

// Message is a patient-facing instruction. The text is shown verbatim. An AI
// layer may paraphrase it for readability or translate it, but may never
// change its meaning, and the original must always stay visible.
type Message struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	Body         string       `json:"body"`
	ReviewStatus ReviewStatus `json:"review_status"`
}

// Catalog holds every message the engine can emit. IDs are versioned: change
// the text, change the ID suffix, so audit records keep pointing at the exact
// wording the patient saw.
//
// ALL TEXT BELOW IS DRAFT. It is a starting point for clinician review, not
// approved medical guidance.
var Catalog = map[string]Message{
	"EMG_NO_ORAL_001": {
		Title: "Emergency — call 112 now",
		Body: "Do not give any food, drink or tablets by mouth. Someone nearby should call 112 now. " +
			"If glucagon has been prescribed and someone is trained to give it, follow the emergency plan. " +
			"Place the person on their side and stay with them.",
	},
	"EMG_HYPER_001": {
		Title: "Get emergency care now",
		Body: "Your glucose is high and you have warning symptoms. Call 112 or go to the nearest emergency department now. " +
			"Do not wait for another reading or for this app to respond.",
	},
	"URG_REDFLAG_001": {
		Title: "These symptoms need urgent attention",
		Body: "Contact your care team now. If symptoms are severe or getting worse, call 112. " +
			"Do not wait for another reading before getting help.",
	},
	"SEVERE_LOW_001": {
		Title: "Very low glucose — treat now",
		Body: "If you are awake and can swallow safely, take 15 g of fast-acting sugar now " +
			"(for example 150 ml fruit juice or regular soft drink, 1 tablespoon of sugar in water, or glucose tablets totalling 15 g). " +
			"Tell someone near you. Recheck in 15 minutes. If you become drowsy, confused or cannot swallow, someone must call 112.",
	},
	"LOW_001": {
		Title: "Low glucose — treat now",
		Body: "If you are awake and can swallow safely, take 15 g of fast-acting sugar " +
			"(for example 150 ml fruit juice or regular soft drink, 1 tablespoon of sugar in water, or glucose tablets totalling 15 g). " +
			"Rest and recheck in 15 minutes. Once your glucose is back in range, eat a snack or meal if your next meal is more than an hour away.",
	},
	"LOW_PERSIST_001": {
		Title: "Still low after treatment — get help",
		Body: "Your glucose is still low after repeated treatment. Take another 15 g of fast-acting sugar if you can swallow safely, " +
			"and contact your care team now. If you feel worse, become confused or cannot swallow, someone should call 112.",
	},
	"STALE_001": {
		Title: "This reading is not current",
		Body: "The last reading is too old to act on. Check your glucose now with your meter. " +
			"If you feel unwell, follow your plan for how you feel rather than this old number.",
	},
	"STALE_LOW_001": {
		Title: "Last reading was low and is out of date",
		Body: "Your last reading was low, but it is too old to show your glucose now. Check with your meter immediately. " +
			"If you cannot check and you feel shaky, sweaty, dizzy or confused, treat it as low.",
	},
	"FUTURE_TS_001": {
		Title: "Check your device time",
		Body:  "This reading has a time in the future, so it cannot be used. Check the date and time on your meter or sensor, then enter a new reading.",
	},
	"VERIFY_001": {
		Title: "Please confirm this reading",
		Body: "This reading is unusual. Wash and dry your hands and recheck with your meter now. " +
			"Do not ignore it. Follow the guidance shown until you have a confirmed reading.",
	},
	"CONFLICT_001": {
		Title: "Your symptoms don't match the reading",
		Body: "You have symptoms of low glucose but the reading is not low. Wash your hands and recheck now with a fingerstick. " +
			"If you cannot check and still feel symptoms, treat it as low.",
	},
	"HIGH_PLAN_001": {
		Title: "Glucose is above your target",
		Body: "Follow your high-glucose plan from your care team. Drink water if you are able to, and recheck at the time shown. " +
			"Do not change your medicine unless your plan tells you to. Get help if you develop vomiting, stomach pain, difficulty breathing or confusion.",
	},
	"VERY_HIGH_PLAN_001": {
		Title: "Glucose is very high",
		Body: "Follow your high-glucose or sick-day plan, including ketone testing if your plan includes it. " +
			"Contact your care team if it stays this high at your next check. Do not change your medicine unless your plan tells you to. " +
			"Get emergency care if you develop vomiting, stomach pain, difficulty breathing or confusion.",
	},
	"HIGH_NO_PLAN_001": {
		Title: "Glucose is high — contact your care team",
		Body: "You don't have an approved high-glucose plan in the app yet. Contact your care team for advice about this reading, " +
			"and ask them to set up your plan. Get emergency care if you develop vomiting, stomach pain, difficulty breathing or confusion.",
	},
	"SPECIAL_OOR_001": {
		Title: "Out of range — follow your personal plan",
		Body: "Your care needs a plan made specifically for you, so this app does not give general instructions. " +
			"Follow the plan your care team gave you and contact them now. If you feel very unwell, call 112.",
	},
	"SPECIAL_ROUTE_001": {
		Title: "Reading saved",
		Body:  "Your reading has been saved and will appear in your report. Follow the plan your care team gave you.",
	},
	"IN_RANGE_001": {
		Title: "In your target range",
		Body:  "Your reading is within your target range. Keep following your usual plan.",
	},
	"NO_PLAN_001": {
		Title: "Set up your care plan",
		Body:  "Your targets have not been set by a clinician yet. Ask your care team to review and approve your plan in the app.",
	},
}

func init() {
	for id, m := range Catalog {
		m.ID = id
		if m.ReviewStatus == "" {
			m.ReviewStatus = ReviewDraft
		}
		Catalog[id] = m
	}
}

// AllMessagesApproved reports whether every message has clinician sign-off.
func AllMessagesApproved() bool {
	for _, m := range Catalog {
		if m.ReviewStatus != ReviewApproved {
			return false
		}
	}
	return true
}
