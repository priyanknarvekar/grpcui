package standalone

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeEcho returns a handler that responds with a fixed body so we can verify
// sub-handler delegation without a real gRPC connection.
func makeEcho(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
	})
}

// --------------------------------------------------------------------------
// ParseTargets
// --------------------------------------------------------------------------

func TestParseTargets_Empty(t *testing.T) {
	eps := ParseTargets(nil)
	if len(eps) != 0 {
		t.Fatalf("want 0 endpoints, got %d", len(eps))
	}
}

func TestParseTargets_Multiple(t *testing.T) {
	targets := []string{"localhost:9080", "localhost:9081", "myhost:50051"}
	eps := ParseTargets(targets)
	if len(eps) != len(targets) {
		t.Fatalf("want %d endpoints, got %d", len(targets), len(eps))
	}
	for i, ep := range eps {
		if ep.Target != targets[i] {
			t.Errorf("ep[%d].Target: want %q, got %q", i, targets[i], ep.Target)
		}
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — selector page
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_SelectorPage_ContainsEndpoints(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "remotehost:50051", Handler: makeEcho("svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, ep := range endpoints {
		if !strings.Contains(body, ep.Target) {
			t.Errorf("selector page missing endpoint %q", ep.Target)
		}
	}
	// Links to sub-paths should be present
	if !strings.Contains(body, "/endpoint/0/") {
		t.Error("selector page missing link /endpoint/0/")
	}
	if !strings.Contains(body, "/endpoint/1/") {
		t.Error("selector page missing link /endpoint/1/")
	}
}

func TestMultiEndpointHandler_SelectorPage_ContentType(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "localhost:9081", Handler: makeEcho("svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("want text/html Content-Type, got %q", ct)
	}
}

func TestMultiEndpointHandler_SelectorPage_DefaultTitle(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "localhost:9081", Handler: makeEcho("svc1")},
	}
	// No title option — should fall back to "gRPC Web UI".
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "gRPC Web UI") {
		t.Error("selector page missing default title 'gRPC Web UI'")
	}
}

func TestMultiEndpointHandler_SelectorPage_CustomTitle(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "localhost:9081", Handler: makeEcho("svc1")},
	}
	h := MultiEndpointHandler(endpoints, WithMultiTitle("My gRPC Console"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "My gRPC Console") {
		t.Error("selector page missing custom title 'My gRPC Console'")
	}
	// Should NOT contain the default title.
	if strings.Contains(body, "gRPC Web UI") {
		t.Error("selector page should not contain default title when custom title is set")
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — single-endpoint auto-redirect
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_SingleEndpoint_Redirect(t *testing.T) {
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("want %d, got %d", http.StatusTemporaryRedirect, rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/endpoint/0/" {
		t.Errorf("want redirect to /endpoint/0/, got %q", loc)
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — sub-handler delegation
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_SubHandlerDelegation(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("response-from-svc0")},
		{Target: "localhost:9081", Handler: makeEcho("response-from-svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	for i, ep := range endpoints {
		t.Run(fmt.Sprintf("endpoint_%d", i), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/endpoint/%d/", i), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d", rec.Code)
			}
			body := rec.Body.String()
			want := fmt.Sprintf("response-from-svc%d", i)
			_ = ep
			if body != want {
				t.Errorf("want body %q, got %q", want, body)
			}
		})
	}
}

func TestMultiEndpointHandler_SubHandlerStripsPrefix(t *testing.T) {
	// Verify that the sub-handler receives the path *without* the
	// /endpoint/<idx> prefix so that paths like /invoke/ still work.
	var capturedPath string
	recording := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		fmt.Fprint(w, "ok")
	})

	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "localhost:9080", Handler: recording},
	})

	req := httptest.NewRequest(http.MethodGet, "/endpoint/0/invoke/MyService.MyMethod", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if capturedPath != "/invoke/MyService.MyMethod" {
		t.Errorf("want capturedPath %q, got %q", "/invoke/MyService.MyMethod", capturedPath)
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — /switch handler
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_Switch_ValidIndex(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "localhost:9081", Handler: makeEcho("svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/switch?ep=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want %d, got %d", http.StatusSeeOther, rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/endpoint/1/" {
		t.Errorf("want Location /endpoint/1/, got %q", loc)
	}
}

func TestMultiEndpointHandler_Switch_InvalidIndex(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
	}
	h := MultiEndpointHandler(endpoints)

	for _, tc := range []struct {
		name string
		ep   string
	}{
		{"out_of_range", "999"},
		{"negative", "-1"},
		{"not_a_number", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/switch?ep="+tc.ep, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", rec.Code)
			}
		})
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — unknown paths
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_NotFound(t *testing.T) {
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
	})

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — bare /endpoint/<idx> redirect
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_BareSubPathRedirect(t *testing.T) {
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("svc0")},
		{Target: "localhost:9081", Handler: makeEcho("svc1")},
	})

	req := httptest.NewRequest(http.MethodGet, "/endpoint/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("want 301, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/endpoint/1/" {
		t.Errorf("want /endpoint/1/, got %q", loc)
	}
}
