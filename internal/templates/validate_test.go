package templates

import (
	"strings"
	"testing"
)

func TestValidateTextSyntax(t *testing.T) {
	if err := ValidateTextSyntax("subject", "hi {{.name}}"); err != nil {
		t.Errorf("valid template rejected: %v", err)
	}
	err := ValidateTextSyntax("subject", "hi {{.name")
	if err == nil {
		t.Fatal("broken template accepted")
	}
	if !strings.Contains(err.Error(), "unclosed") && !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("expected a parse error, got %v", err)
	}
}

// ValidateHTMLSyntax must catch both text/template syntax errors and
// html/template-specific issues. html/template's parser is strictly
// stricter; regression here would let a bad template slip past upsert
// and surface as a DLQ at send-time instead of a 400 at authoring-time.
func TestValidateHTMLSyntax(t *testing.T) {
	if err := ValidateHTMLSyntax("body", "<p>hi {{.name}}</p>"); err != nil {
		t.Errorf("valid html template rejected: %v", err)
	}
	if err := ValidateHTMLSyntax("body", "<p>hi {{.name"); err == nil {
		t.Error("broken html template accepted")
	}
	// html/template-specific: the `{{.}}` inside a <script> requires
	// JSEscape context; broken quoting trips html/template's parser.
	// (Note: some breakages are only caught at execute time, so this is
	// a best-effort check — we're asserting the parser rejects the
	// clearly-broken form.)
	if err := ValidateHTMLSyntax("body", "<p>{{ if }}</p>"); err == nil {
		t.Error("broken html template with bad action accepted")
	}
}
