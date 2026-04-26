package templates

import (
	"strings"
	"testing"
)

func TestValidate_MissingAndPresent(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		vars     map[string]any
		want     []string
	}{
		{"all missing", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"one missing", []string{"a", "b"}, map[string]any{"a": "x"}, []string{"b"}},
		{"all present", []string{"a", "b"}, map[string]any{"a": "x", "b": "y"}, []string{}},
		{"empty string is missing", []string{"a"}, map[string]any{"a": ""}, []string{"a"}},
		{"none required", nil, map[string]any{"x": "y"}, []string{}},
		{"blank required keys ignored", []string{""}, nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Validate(tc.required, tc.vars)
			if !equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRender_AllSections(t *testing.T) {
	s := Spec{
		Name:              "welcome",
		Subject:           "Welcome {{.name}}",
		BodyText:          "Hi {{.name}}, enjoy {{.product}}.",
		BodyHTML:          "<p>Hi <b>{{.name}}</b>.</p>",
		RequiredVariables: []string{"name", "product"},
	}
	r, err := Render(s, map[string]any{"name": "Ada", "product": "Widgets"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.Subject != "Welcome Ada" {
		t.Errorf("subject=%q", r.Subject)
	}
	if !strings.Contains(r.BodyText, "Ada") || !strings.Contains(r.BodyText, "Widgets") {
		t.Errorf("text=%q", r.BodyText)
	}
	if !strings.Contains(r.BodyHTML, "<b>Ada</b>") {
		t.Errorf("html=%q", r.BodyHTML)
	}
}

func TestRender_HTMLAutoescapes(t *testing.T) {
	s := Spec{BodyText: "ignored", BodyHTML: "<p>{{.name}}</p>"}
	r, err := Render(s, map[string]any{"name": "<script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.BodyHTML, "<script>") {
		t.Errorf("html not escaped: %q", r.BodyHTML)
	}
}

func TestRender_MissingVarReturnsError(t *testing.T) {
	s := Spec{BodyText: "Hi {{.name}}", RequiredVariables: []string{"name"}}
	_, err := Render(s, map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("err=%v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
