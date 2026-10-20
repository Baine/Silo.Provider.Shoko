package plugin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func newWeb(t *testing.T, baseURL string) *Web {
	t.Helper()
	server := NewServer()
	if baseURL != "" {
		configure(t, server, map[string]any{"base_url": baseURL})
	}
	return NewWeb(server)
}

func get(t *testing.T, web *Web) *pluginv1.HandleHTTPResponse {
	t.Helper()
	response, err := web.Handle(t.Context(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet,
		Path:   connectionTestPath,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return response
}

func post(t *testing.T, web *Web, values url.Values) string {
	t.Helper()
	response, err := web.Handle(t.Context(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodPost,
		Path:   connectionTestPath,
		Body:   []byte(values.Encode()),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if response.GetStatusCode() != http.StatusOK {
		t.Fatalf("status = %d", response.GetStatusCode())
	}
	return string(response.GetBody())
}

func TestConnectionPagePrefillsSavedURL(t *testing.T) {
	body := string(get(t, newWeb(t, "http://shoko:8111")).GetBody())
	if !strings.Contains(body, `value="http://shoko:8111"`) {
		t.Fatalf("the saved URL is not prefilled:\n%s", body)
	}
	if !strings.Contains(body, `name="api_key"`) {
		t.Fatal("the form has no API key field")
	}
}

func TestConnectionPageReportsRejectedKey(t *testing.T) {
	shokoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(shokoServer.Close)

	body := post(t, newWeb(t, ""), url.Values{
		"base_url": {shokoServer.URL},
		"api_key":  {"wrong"},
	})
	if !strings.Contains(body, "Shoko rejected the API key") {
		t.Fatalf("the page did not separate a rejected key from an unreachable server:\n%s", body)
	}
}

func TestConnectionPageReportsCounts(t *testing.T) {
	shokoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/User/Current":
			_, _ = w.Write([]byte(`{"ID":3,"Username":"silo"}`))
		case r.URL.Path == "/api/v3/Series":
			_, _ = w.Write([]byte(`{"Total":12,"List":[{}]}`))
		case r.URL.Query().Get("includeWatched") == "Only":
			_, _ = w.Write([]byte(`{"Total":40,"List":[{}]}`))
		default:
			_, _ = w.Write([]byte(`{"Total":300,"List":[{}]}`))
		}
	}))
	t.Cleanup(shokoServer.Close)

	body := post(t, newWeb(t, ""), url.Values{
		"base_url": {shokoServer.URL},
		"api_key":  {"good"},
	})
	for _, want := range []string{"Signed in as silo", "40 watched of 300 episodes", "<dd>12</dd>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "good") {
		t.Fatal("the page echoed the API key back")
	}
}

func TestConnectionPageRejectsOtherPaths(t *testing.T) {
	response, err := newWeb(t, "").Handle(t.Context(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodGet,
		Path:   "/assets/app.js",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if response.GetStatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d", response.GetStatusCode())
	}
}
