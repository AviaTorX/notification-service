// Package templates renders notification content.
//
// Two engines run side by side:
//   - text/template for plain-text bodies and Slack messages.
//   - html/template for email HTML bodies (auto-escapes, protects against XSS).
//
// Required-variable validation happens BEFORE rendering so we can fail fast
// with a clear error ("missing variable X") rather than surfacing a
// template-engine error that mentions `<no value>`.
//
// Parsed templates are cached by content hash (SHA-256 of the source) so
// high-volume workloads don't burn CPU re-parsing the same template on
// every send. Cache grows unbounded in principle; in practice template
// sources are few and stable, and adding an LRU would be cheap if this
// ever becomes a memory concern.
package templates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	htmltpl "html/template"
	"sort"
	"strings"
	"sync"
	texttpl "text/template"
)

type Rendered struct {
	Subject  string
	BodyText string
	BodyHTML string
}

type Spec struct {
	Name              string
	Subject           string
	BodyText          string
	BodyHTML          string
	RequiredVariables []string
}

// Validate returns a sorted, deduplicated list of missing required variables.
// Empty-string values are treated as missing — same behaviour as a missing key.
func Validate(required []string, vars map[string]any) []string {
	missing := map[string]struct{}{}
	for _, key := range required {
		if key == "" {
			continue
		}
		v, ok := vars[key]
		if !ok {
			missing[key] = struct{}{}
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			missing[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(missing))
	for k := range missing {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func Render(s Spec, vars map[string]any) (Rendered, error) {
	if miss := Validate(s.RequiredVariables, vars); len(miss) > 0 {
		return Rendered{}, fmt.Errorf("missing required variables: %s", strings.Join(miss, ","))
	}
	if vars == nil {
		vars = map[string]any{}
	}

	var out Rendered

	if s.Subject != "" {
		subj, err := renderText("subject", s.Subject, vars)
		if err != nil {
			return Rendered{}, fmt.Errorf("render subject: %w", err)
		}
		out.Subject = subj
	}

	body, err := renderText("body_text", s.BodyText, vars)
	if err != nil {
		return Rendered{}, fmt.Errorf("render body_text: %w", err)
	}
	out.BodyText = body

	if s.BodyHTML != "" {
		html, err := renderHTML("body_html", s.BodyHTML, vars)
		if err != nil {
			return Rendered{}, fmt.Errorf("render body_html: %w", err)
		}
		out.BodyHTML = html
	}

	return out, nil
}

// RenderInline renders a single template body (used for Slack + in-app title
// construction) without requiring a full Spec.
func RenderInline(name, tmpl string, vars map[string]any) (string, error) {
	return renderText(name, tmpl, vars)
}

// ValidateTextSyntax returns nil if the given source parses as a text/template.
func ValidateTextSyntax(name, src string) error {
	_, err := texttpl.New(name).Option("missingkey=error").Parse(src)
	return err
}

// ValidateHTMLSyntax returns nil if the given source parses as an
// html/template.
func ValidateHTMLSyntax(name, src string) error {
	_, err := htmltpl.New(name).Option("missingkey=error").Parse(src)
	return err
}

// --- parse-cache plumbing ---

var (
	textCacheMu sync.RWMutex
	textCache   = map[string]*texttpl.Template{}

	htmlCacheMu sync.RWMutex
	htmlCache   = map[string]*htmltpl.Template{}
)

func sourceKey(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// parseTextCached returns a parsed text/template, reusing a previously
// parsed instance when the source hash matches. text/template is safe to
// Execute() concurrently against the same parsed *Template.
func parseTextCached(name, src string) (*texttpl.Template, error) {
	key := sourceKey(src)
	textCacheMu.RLock()
	if t, ok := textCache[key]; ok {
		textCacheMu.RUnlock()
		return t, nil
	}
	textCacheMu.RUnlock()

	t, err := texttpl.New(name).Option("missingkey=error").Parse(src)
	if err != nil {
		return nil, err
	}
	textCacheMu.Lock()
	// Tolerate a concurrent insert by the same key — both parses are
	// semantically identical, last writer wins is fine.
	textCache[key] = t
	textCacheMu.Unlock()
	return t, nil
}

func parseHTMLCached(name, src string) (*htmltpl.Template, error) {
	key := sourceKey(src)
	htmlCacheMu.RLock()
	if t, ok := htmlCache[key]; ok {
		htmlCacheMu.RUnlock()
		return t, nil
	}
	htmlCacheMu.RUnlock()

	t, err := htmltpl.New(name).Option("missingkey=error").Parse(src)
	if err != nil {
		return nil, err
	}
	htmlCacheMu.Lock()
	htmlCache[key] = t
	htmlCacheMu.Unlock()
	return t, nil
}

func renderText(name, src string, vars map[string]any) (string, error) {
	t, err := parseTextCached(name, src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderHTML(name, src string, vars map[string]any) (string, error) {
	t, err := parseHTMLCached(name, src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}
