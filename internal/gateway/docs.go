package gateway

import (
	"html/template"
	"net/http"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

// decisionQuestionsDoc is OpenRouter's question schema (DecisionsRequest in https://openrouter.ai/openapi.json).
const decisionQuestionsDoc = `{"<key>": question}. Every question needs "type" and "instructions" (string, object or array). ` +
	`noul (yes/no): optional "criteria": {"true": "...", "false": "..."}; the answer "noul" is the probability of true (0-1). ` +
	`choice: "criteria": {"<option>": "<when to pick it>", ...} (an object, not a list); the answer is "choice": "<option>" with "probabilities" per option. ` +
	`score: "criteria": ["<lowest>", ..., "<highest>"] (ordered, at least one); the answer "score" is a number from 0 to len-1 with a "legend". ` +
	`box: instructions only (needs an image asset); the answer "box" is {top, bottom, left, right}.`

func errorResponse(desc string) map[string]any {
	return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{
		"schema": map[string]any{"$ref": "#/components/schemas/Error"},
	}}}
}

// openapiSpec describes only this deployment's endpoints.
func (g *Gateway) openapiSpec() map[string]any {
	attribution := []any{
		map[string]any{"name": SurfaceHeader, "in": "header", "required": true, "description": "Where the call comes from, e.g. internal, slack, bean. 1-64 of [a-z0-9._-].", "schema": map[string]any{"type": "string"}},
		map[string]any{"name": AgentHeader, "in": "header", "required": true, "description": "A stable name for your app or agent. 1-64 of [a-z0-9._-].", "schema": map[string]any{"type": "string"}},
	}
	errs := map[string]any{
		"400": errorResponse("Missing/invalid X-AGENT-* header, model not allowed, or upstream rejected the request"),
		"401": errorResponse("Missing, unknown or revoked key"),
		"402": errorResponse("Key budget, service monthly cap or the shared OpenRouter credit floor reached"),
		"403": errorResponse("Key is for the other service, or lacks the scope"),
		"413": errorResponse("Request exceeds the key's max tokens per request"),
		"422": errorResponse("Invalid request body"),
		"429": errorResponse("Rate limited (see Retry-After)"),
		"502": errorResponse("OpenRouter unreachable or refused the gateway"),
		"503": errorResponse("OpenRouter rate limiting, or a dependency is down"),
	}
	models := g.Cfg.Models(g.service())
	var op map[string]any
	if g.service() == config.Barista {
		op = map[string]any{
			"summary":     "Ask a decision model (OpenRouter Decisions API)",
			"description": "Body is passed to OpenRouter's Decisions API. provider.data_collection is forced by the gateway. Allowed models: first is the default.",
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type": "object", "required": []string{"state", "questions"}, "additionalProperties": true,
				"properties": map[string]any{
					"model":     map[string]any{"type": "string", "enum": models},
					"state":     map[string]any{"description": "The situation to decide about (string or JSON)."},
					"questions": map[string]any{"type": "object", "description": decisionQuestionsDoc},
					"assets": map[string]any{"type": "object", "description": `Images keyed by reference: {"<ref>": {"type": "image", ` +
						`"format": "png|jpg|jpeg", "data_base64": "<base64, no data: prefix>"}}. Use "<ref>" as a whole string ` +
						`in state, instructions or criteria; every asset must be referenced.`},
				},
			}}}},
		}
	} else {
		op = map[string]any{
			"summary":     "Embed text (OpenRouter /embeddings)",
			"description": "OpenAI-style embeddings request. provider.data_collection is forced by the gateway.",
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type": "object", "required": []string{"input"}, "additionalProperties": true,
				"properties": map[string]any{
					"model": map[string]any{"type": "string", "enum": models},
					"input": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": g.Cfg.MaxEmbedInputs}}},
				},
			}}}},
		}
	}
	responses := map[string]any{"200": map[string]any{
		"description": "OpenRouter's response, unchanged",
		"headers": map[string]any{
			"X-Cost-Usd":             map[string]any{"schema": map[string]any{"type": "string"}, "description": "Cost of this call"},
			"X-Budget-Remaining-Usd": map[string]any{"schema": map[string]any{"type": "string"}, "description": "Left in your key's monthly budget"},
		},
	}}
	for k, v := range errs {
		responses[k] = v
	}
	op["parameters"] = attribution
	op["responses"] = responses
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "cafe-gateway: " + string(g.service()),
			"version":     "1",
			"description": "Authenticate with 'Authorization: Bearer " + g.service().KeyPrefix() + "_…'. Every model call needs X-AGENT-SURFACE and X-AGENT-ID.",
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
			"schemas": map[string]any{"Error": map[string]any{"type": "object", "properties": map[string]any{
				"error": map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{
					"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
				}},
			}}},
		},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths": map[string]any{
			g.service().Endpoint(): map[string]any{"post": op},
			"/v1/usage": map[string]any{"get": map[string]any{
				"summary": "Your key's usage this month",
				"parameters": []any{
					map[string]any{"name": "month", "in": "query", "schema": map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}$`}},
					map[string]any{"name": "by", "in": "query", "schema": map[string]any{"type": "string", "enum": ownUsageGroups}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "Usage report"}, "401": errorResponse("Bad key")},
			}},
			"/health": map[string]any{"get": map[string]any{"summary": "Liveness", "security": []any{}, "responses": map[string]any{"200": map[string]any{"description": "ok"}}}},
		},
	}
}

func (g *Gateway) openapi(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.openapiSpec())
}

var docsTemplate = template.Must(template.New("docs").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>cafe-gateway: {{.Service}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;color:#222;background:#fff}
pre{background:#f4f1ec;padding:1rem;overflow-x:auto;border-radius:6px}code{font-size:.9em}
@media (prefers-color-scheme:dark){body{color:#eee;background:#1b1a19}pre{background:#2a2826}}</style></head>
<body><h1>cafe-gateway: {{.Service}}</h1>
<p>{{if eq .Service "barista"}}Decisions (OpenRouter Decisions API){{else}}Embeddings (OpenRouter /embeddings){{end}}.
Your key starts with <code>{{.Prefix}}_</code>. Default model <code>{{index .Models 0}}</code>; allowed: {{range .Models}}<code>{{.}}</code> {{end}}</p>
<p>Every call needs <code>X-AGENT-SURFACE</code> (where it comes from) and <code>X-AGENT-ID</code> (your app/agent name).
The response carries <code>X-Cost-Usd</code> and <code>X-Budget-Remaining-Usd</code>.</p>
<pre><code>{{.Example}}</code></pre>
{{if eq .Service "barista"}}<p>Question types: every question needs <code>type</code> and <code>instructions</code>.
<code>noul</code> (yes/no; optional <code>criteria</code> <code>{"true": …, "false": …}</code>; answer = probability of true),
<code>choice</code> (<code>criteria</code> is an object <code>{"option": "when to pick it"}</code>),
<code>score</code> (<code>criteria</code> is an ordered list, lowest first; answer is 0…n-1),
<code>box</code> (instructions only, with an image asset; answer is <code>{top, bottom, left, right}</code>).</p>{{end}}
<p>Your usage this month: <code>GET /v1/usage?by=surface</code>. Machine-readable spec: <a href="/openapi.json">/openapi.json</a>.</p>
</body></html>`))

func (g *Gateway) docs(w http.ResponseWriter, r *http.Request) {
	example := `curl -s https://$HOST/v1/decide \
  -H "Authorization: Bearer $BARISTA_KEY" \
  -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: my-script" \
  -H "Content-Type: application/json" \
  -d '{
    "state": "3 open bugs, demo tomorrow",
    "questions": {
      "ship": {"type": "noul", "instructions": "Should we ship today?",
               "criteria": {"true": "The bugs are minor.", "false": "A bug would break the demo."}},
      "first": {"type": "choice", "instructions": "Which bug first?",
                "criteria": {"login": "Users can't sign in.", "export": "CSV export is wrong."}},
      "urgency": {"type": "score", "instructions": "How urgent is the fix?",
                  "criteria": ["Can wait", "This week", "Right now"]}
    }
  }'`
	if g.service() == config.Grinder {
		example = `curl -s https://$HOST/v1/embed \
  -H "Authorization: Bearer $GRINDER_KEY" \
  -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: my-script" \
  -H "Content-Type: application/json" \
  -d '{"input": ["espresso", "latte"]}'`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = docsTemplate.Execute(w, map[string]any{
		"Service": string(g.service()), "Prefix": g.service().KeyPrefix(), "Models": g.Cfg.Models(g.service()),
		"Example": example,
	})
	_ = r
}
