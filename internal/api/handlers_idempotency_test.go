package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/avinash-shinde/notification-service/internal/store"
)

// buildCreateParams freezes the template snapshot at request time. If
// the template is later edited or deleted, the notification's delivered
// content must still match what the caller saw when they accepted the
// 202.
func TestBuildCreateParams_SnapshotsTemplateContent(t *testing.T) {
	subj := "hi {{.name}}"
	html := "<p>hi</p>"
	tmpl := &store.Template{
		ID:                uuid.New(),
		Name:              "welcome",
		Subject:           &subj,
		BodyText:          "hi {{.name}}",
		BodyHTML:          &html,
		RequiredVariables: []string{"name"},
	}
	req := createNotificationReq{
		UserID:    uuid.New().String(),
		Variables: map[string]any{"name": "Ada"},
	}
	p := buildCreateParams(uuid.New(), tmpl, []string{"email"}, req, "idem-abc")

	if p.TemplateNameSnapshot == nil || *p.TemplateNameSnapshot != "welcome" {
		t.Errorf("template name snapshot missing or wrong: %+v", p.TemplateNameSnapshot)
	}
	if p.BodyTextSnapshot == nil || *p.BodyTextSnapshot != tmpl.BodyText {
		t.Errorf("body snapshot mismatch")
	}
	if p.SubjectSnapshot == nil || *p.SubjectSnapshot != subj {
		t.Errorf("subject snapshot mismatch")
	}
	if p.BodyHTMLSnapshot == nil || *p.BodyHTMLSnapshot != html {
		t.Errorf("html snapshot mismatch")
	}
	if len(p.RequiredVariablesSnapshot) != 1 || p.RequiredVariablesSnapshot[0] != "name" {
		t.Errorf("required vars snapshot mismatch: %v", p.RequiredVariablesSnapshot)
	}
	if p.IdempotencyKey == nil || *p.IdempotencyKey != "idem-abc" {
		t.Errorf("idempotency key not attached")
	}
	// Mutating the original template must not affect the snapshotted params.
	origRV := make([]string, len(p.RequiredVariablesSnapshot))
	copy(origRV, p.RequiredVariablesSnapshot)
	tmpl.RequiredVariables[0] = "mutated"
	if p.RequiredVariablesSnapshot[0] != origRV[0] {
		t.Errorf("snapshot must be a copy, not a shared slice")
	}
}

func TestBuildCreateParams_NoTemplate_SkipsSnapshotFields(t *testing.T) {
	req := createNotificationReq{
		UserID:    uuid.New().String(),
		Variables: map[string]any{"subject": "raw", "body_text": "hello"},
	}
	p := buildCreateParams(uuid.New(), nil, []string{"email"}, req, "")
	if p.TemplateID != nil || p.TemplateNameSnapshot != nil ||
		p.BodyTextSnapshot != nil || p.BodyHTMLSnapshot != nil ||
		p.SubjectSnapshot != nil || len(p.RequiredVariablesSnapshot) != 0 {
		t.Errorf("ad-hoc request should not populate snapshot fields: %+v", p)
	}
	if p.IdempotencyKey != nil {
		t.Errorf("empty idem key should not be attached")
	}
}
