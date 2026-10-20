package plugin

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/Baine/silo-plugin-shoko/internal/shoko"
)

// connectionTestPath is the single route the manifest advertises. The host
// proxies it under /plugins/{installation_id}, so the page only has to render
// relative URLs.
const connectionTestPath = "/connection-test"

// Web serves the plugin's HTTP routes. It exists so an operator can check the
// server URL and an API key without waiting for an hourly sync to fail: the
// sync reports a rejected key as a generic failure in a log, while this page
// separates "the server is unreachable" from "the key is wrong".
type Web struct {
	pluginv1.UnimplementedHttpRoutesServer

	provider *Server
}

// NewWeb returns the HTTP route handler for a provider.
func NewWeb(provider *Server) *Web { return &Web{provider: provider} }

// Handle answers the connection test page.
func (w *Web) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if !strings.HasSuffix(strings.TrimSuffix(req.GetPath(), "/"), connectionTestPath) && req.GetPath() != connectionTestPath {
		return notFound(), nil
	}

	switch req.GetMethod() {
	case http.MethodGet:
		return htmlResponse(http.StatusOK, w.renderForm("", "", "")), nil
	case http.MethodPost:
		form, err := url.ParseQuery(string(req.GetBody()))
		if err != nil {
			return htmlResponse(http.StatusBadRequest, w.renderForm("", "", "The form could not be read.")), nil
		}
		report := w.test(ctx, form.Get("base_url"), form.Get("api_key"))
		return htmlResponse(http.StatusOK, w.renderForm(form.Get("base_url"), "", report)), nil
	default:
		return &pluginv1.HandleHTTPResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
			Body:       []byte("method not allowed"),
		}, nil
	}
}

// test probes the server the operator typed, falling back to the saved URL when
// the form field is empty.
func (w *Web) test(ctx context.Context, rawURL, apiKey string) string {
	baseURL := strings.TrimSpace(rawURL)
	if baseURL == "" {
		baseURL = w.provider.configuration().baseURL
	}
	if baseURL == "" {
		return failure("No Shoko server URL is set", "Save a URL on the plugin settings page first, then test again.")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return failure("No API key entered", "Paste the key from Shoko Server under Settings → API Keys.")
	}

	client, err := shoko.New(baseURL, apiKey)
	if err != nil {
		return failure("The server URL is not usable", err.Error())
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return failure(unreachableTitle(err), unreachableHint(err))
	}
	stats, err := client.Stats(ctx)
	if err != nil {
		return success(
			fmt.Sprintf("Connected as %s", user.Username),
			"The key is accepted, but Shoko did not return counts: "+describe(err)+".",
			nil,
		)
	}

	rows := []reportRow{
		{"Signed in as", user.Username},
		{"Series", fmt.Sprintf("%d", stats.Series)},
		{"Episodes", fmt.Sprintf("%d", stats.Episodes)},
		{"Watched episodes", fmt.Sprintf("%d", stats.Watched)},
		{"Numbering", w.provider.configuration().numbering},
	}
	summary := fmt.Sprintf("%d watched of %d episodes", stats.Watched, stats.Episodes)
	if stats.Watched == 0 {
		return success("Signed in as "+user.Username, "The key works, but Shoko reports no watched episodes, so a sync would import nothing.", rows)
	}
	return success("Signed in as "+user.Username, "The key works. Shoko reports "+summary+" for this account.", rows)
}

// unreachableTitle turns a client error into the part an operator has to act
// on.
func unreachableTitle(err error) string {
	switch {
	case isUnauthorized(err):
		return "Shoko rejected the API key"
	case isNotFound(err):
		return "The server answered, but not with the Shoko API"
	default:
		return "The server could not be reached"
	}
}

func unreachableHint(err error) string {
	switch {
	case isUnauthorized(err):
		return "Create a key in Shoko Server under Settings → API Keys, and check it belongs to the user whose history you want."
	case isNotFound(err):
		return "Check the URL points at the Shoko server itself, including the port, without a path after it."
	default:
		return "Check the URL is reachable from the Silo server, that Shoko is running, and that the port is open. " +
			describe(err)
	}
}

func isUnauthorized(err error) bool { return strings.Contains(err.Error(), "rejected the API key") }

func isNotFound(err error) bool { return strings.Contains(err.Error(), "no such record") }

// describe reports the error without any provider response body, which can
// carry server internals.
func describe(err error) string {
	var apiErr *shoko.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("HTTP %d", apiErr.StatusCode)
	}
	return err.Error()
}

type reportRow struct {
	label string
	value string
}

func success(title, detail string, rows []reportRow) string {
	var body strings.Builder
	body.WriteString(`<section class="card ok">`)
	body.WriteString(`<h2><span class="dot"></span>` + html.EscapeString(title) + `</h2>`)
	body.WriteString(`<p>` + html.EscapeString(detail) + `</p>`)
	if len(rows) > 0 {
		body.WriteString(`<dl>`)
		for _, row := range rows {
			body.WriteString(`<dt>` + html.EscapeString(row.label) + `</dt><dd>` + html.EscapeString(row.value) + `</dd>`)
		}
		body.WriteString(`</dl>`)
	}
	body.WriteString(`</section>`)
	return body.String()
}

func failure(title, detail string) string {
	var body strings.Builder
	body.WriteString(`<section class="card bad">`)
	body.WriteString(`<h2><span class="dot"></span>` + html.EscapeString(title) + `</h2>`)
	body.WriteString(`<p>` + html.EscapeString(detail) + `</p>`)
	body.WriteString(`</section>`)
	return body.String()
}

// renderForm draws the page: the saved settings, the probe form and, once a
// test has run, its report.
func (w *Web) renderForm(baseURL, _ string, report string) string {
	if baseURL == "" {
		baseURL = w.provider.configuration().baseURL
	}
	conf := w.provider.configuration()

	var page strings.Builder
	page.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	page.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	page.WriteString(`<title>Shoko connection</title><style>`)
	page.WriteString(pageStyle)
	page.WriteString(`</style></head><body><main>`)
	page.WriteString(`<h1>Shoko connection</h1>`)
	page.WriteString(`<p class="lede">Checks that the Silo server can reach your Shoko server and that an API key is accepted, without waiting for the next sync.</p>`)

	if report != "" {
		page.WriteString(report)
	}

	page.WriteString(`<section class="card">`)
	page.WriteString(`<h2>Test a key</h2>`)
	page.WriteString(`<form method="post">`)
	page.WriteString(`<label for="base_url">Shoko server URL</label>`)
	page.WriteString(`<input id="base_url" name="base_url" type="url" required value="` + html.EscapeString(baseURL) + `" placeholder="http://shoko:8111">`)
	page.WriteString(`<label for="api_key">API key</label>`)
	page.WriteString(`<input id="api_key" name="api_key" type="password" required autocomplete="off" spellcheck="false">`)
	page.WriteString(`<p class="help">Created in Shoko Server under Settings → API Keys. The key is only used for this request.</p>`)
	page.WriteString(`<button type="submit">Test connection</button>`)
	page.WriteString(`</form></section>`)

	page.WriteString(`<section class="card">`)
	page.WriteString(`<h2>Saved settings</h2>`)
	page.WriteString(`<dl><dt>Server URL</dt><dd>` + valueOrDash(conf.baseURL) + `</dd>`)
	page.WriteString(`<dt>Episode numbering</dt><dd>` + valueOrDash(conf.numbering) + `</dd></dl>`)
	page.WriteString(`<p class="help">Both are changed on this plugin's settings page.</p>`)
	page.WriteString(`</section>`)

	page.WriteString(`</main></body></html>`)
	return page.String()
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return `<span class="muted">not set</span>`
	}
	return html.EscapeString(value)
}

func htmlResponse(status int, body string) *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers: map[string]string{
			"Content-Type": "text/html; charset=utf-8",
			// The page is self-contained and posts back to itself.
			"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'",
			"Cache-Control":           "no-store",
		},
		Body: []byte(body),
	}
}

func notFound() *pluginv1.HandleHTTPResponse {
	return &pluginv1.HandleHTTPResponse{
		StatusCode: http.StatusNotFound,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       []byte("not found"),
	}
}

const pageStyle = `
:root{color-scheme:light dark;--bg:#f7f7f8;--card:#fff;--text:#18181b;--muted:#6b7280;--line:#e4e4e7;--ok:#15803d;--bad:#b91c1c;--accent:#2563eb}
@media (prefers-color-scheme:dark){:root{--bg:#0b0b0c;--card:#151517;--text:#f4f4f5;--muted:#a1a1aa;--line:#27272a;--ok:#4ade80;--bad:#f87171;--accent:#60a5fa}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:44rem;margin:0 auto;padding:2.5rem 1.25rem 4rem}
h1{font-size:1.5rem;margin:0 0 .5rem}
h2{font-size:1rem;margin:0 0 .75rem;display:flex;align-items:center;gap:.5rem}
.lede{color:var(--muted);margin:0 0 1.5rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:.75rem;padding:1.25rem;margin:0 0 1rem}
.card p{margin:.25rem 0 0;color:var(--muted)}
.dot{width:.6rem;height:.6rem;border-radius:50%;background:var(--muted);display:inline-block}
.ok .dot{background:var(--ok)}
.bad .dot{background:var(--bad)}
dl{display:grid;grid-template-columns:auto 1fr;gap:.35rem 1rem;margin:1rem 0 0;font-variant-numeric:tabular-nums}
dt{color:var(--muted)}
dd{margin:0}
label{display:block;margin:1rem 0 .35rem;font-weight:600;font-size:.9rem}
input{width:100%;padding:.6rem .7rem;border:1px solid var(--line);border-radius:.5rem;background:var(--bg);color:var(--text);font:inherit}
input:focus{outline:2px solid var(--accent);outline-offset:1px}
button{margin-top:1.1rem;padding:.6rem 1rem;border-radius:.5rem;border:1px solid var(--accent);background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
.help{color:var(--muted);font-size:.85rem}
.muted{color:var(--muted)}
`
