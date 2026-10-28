package mockopenrouter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/alpha/decisions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-or-test")
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rec.Body)
	}
	return rec.Code, out
}

// readmeDecideExample is the body of the README's barista curl example.
func readmeDecideExample(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)/v1/decide \\.*?-d '(\{.*?\})'\n`).FindSubmatch(readme)
	if m == nil {
		t.Fatal("no /v1/decide curl example in README.md")
	}
	return string(m[1])
}

// The README example must be a valid request under OpenRouter's schema (the bug that reached production was a
// choice question with list criteria).
func TestReadmeExampleIsValid(t *testing.T) {
	status, out := post(t, readmeDecideExample(t))
	if status != 200 {
		t.Fatalf("README example rejected: %d %v", status, out)
	}
	answers := out["answers"].(map[string]any)
	ship := answers["ship"].(map[string]any)
	if _, ok := ship["noul"].(float64); !ok {
		t.Fatalf("noul answer must be a number: %v", ship)
	}
	first := answers["first"].(map[string]any)
	if !slices.Contains([]string{"login", "export", "pricing"}, first["choice"].(string)) {
		t.Fatalf("choice answer must be one of the criteria keys: %v", first)
	}
	urgency := answers["urgency"].(map[string]any)
	if _, ok := urgency["score"].(float64); !ok || len(urgency["legend"].(map[string]any)) != 3 {
		t.Fatalf("score answer: %v", urgency)
	}
}

func TestSchemaErrors(t *testing.T) {
	for name, tc := range map[string]struct{ question, want string }{
		"choice criteria as a list": {`{"type": "choice", "instructions": "x", "criteria": ["a", "b"]}`, "criteria must be an object"},
		"missing instructions":      {`{"type": "noul"}`, "instructions is required"},
		"score without criteria":    {`{"type": "score", "instructions": "x"}`, "non-empty array"},
		"empty score criteria":      {`{"type": "score", "instructions": "x", "criteria": []}`, "non-empty array"},
		"noul criteria incomplete":  {`{"type": "noul", "instructions": "x", "criteria": {"true": "y"}}`, "criteria must be {"},
		"unknown type":              {`{"type": "rank", "instructions": "x"}`, "type must be one of"},
	} {
		t.Run(name, func(t *testing.T) {
			status, out := post(t, `{"model": "m", "state": "s", "questions": {"q": `+tc.question+`}}`)
			raw, _ := json.Marshal(out)
			if status != 400 || !bytes.Contains(raw, []byte(tc.want)) || !bytes.Contains(raw, []byte("questions.q")) {
				t.Fatalf("%d %s", status, raw)
			}
		})
	}
}

func TestValidQuestionShapes(t *testing.T) {
	status, out := post(t, `{"model": "m", "state": {"img": "shot"}, "questions": {
		"n": {"type": "noul", "instructions": {"ask": "is it?"}},
		"b": {"type": "box", "instructions": "Where is the cat? shot"}}}`)
	if status != 200 {
		t.Fatalf("%d %v", status, out)
	}
	box := out["answers"].(map[string]any)["b"].(map[string]any)["box"].(map[string]any)
	for _, k := range []string{"top", "bottom", "left", "right"} {
		if _, ok := box[k]; !ok {
			t.Fatalf("box answer lacks %s: %v", k, box)
		}
	}
}
