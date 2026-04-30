package standalone

import (
	"encoding/json"
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

// makeConnectFuncOK returns a ConnectFunc that always succeeds with an echo handler.
func makeConnectFuncOK(body string) func() (http.Handler, error) {
	return func() (http.Handler, error) {
		return makeEcho(body), nil
	}
}

// makeConnectFuncFail returns a ConnectFunc that always fails.
func makeConnectFuncFail(errMsg string) func() (http.Handler, error) {
	return func() (http.Handler, error) {
		return nil, fmt.Errorf("%s", errMsg)
	}
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "remotehost:50051", ConnectFunc: makeConnectFuncOK("svc1")},
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
}

func TestMultiEndpointHandler_SelectorPage_ContentType(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
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
// MultiEndpointHandler — sub-handler delegation via lazy connect
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_LazyConnectAndDelegation(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("response-from-svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("response-from-svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	for i := range endpoints {
		t.Run(fmt.Sprintf("endpoint_%d", i), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/endpoint/%d/", i), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d (body: %s)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			want := fmt.Sprintf("response-from-svc%d", i)
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
// MultiEndpointHandler — eager handler (backward compat)
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_EagerHandler(t *testing.T) {
	// When Handler is set directly (no ConnectFunc), it should work immediately.
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "localhost:9080", Handler: makeEcho("eager-response")},
		{Target: "localhost:9081", Handler: makeEcho("eager-response2")},
	})

	req := httptest.NewRequest(http.MethodGet, "/endpoint/0/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.String() != "eager-response" {
		t.Errorf("want 'eager-response', got %q", rec.Body.String())
	}
}

// --------------------------------------------------------------------------
// MultiEndpointHandler — /switch handler
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_Switch_ValidIndex(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
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
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
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

// --------------------------------------------------------------------------
// /api/endpoints — status listing
// --------------------------------------------------------------------------

func TestAPIEndpoints_InitialStatus(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: makeConnectFuncOK("a")},
		{Target: "host-b:9081", ConnectFunc: makeConnectFuncOK("b")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/api/endpoints", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	var statuses []endpointJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("want 2 statuses, got %d", len(statuses))
	}
	for i, s := range statuses {
		if s.Status != StatusPending {
			t.Errorf("endpoint[%d]: want status %q, got %q", i, StatusPending, s.Status)
		}
		if s.Target != endpoints[i].Target {
			t.Errorf("endpoint[%d]: want target %q, got %q", i, endpoints[i].Target, s.Target)
		}
	}
}

func TestAPIEndpoints_EagerStatusConnected(t *testing.T) {
	// Endpoints with Handler set directly should show as "connected".
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", Handler: makeEcho("a")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/api/endpoints", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var statuses []endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &statuses)
	if statuses[0].Status != StatusConnected {
		t.Errorf("want status %q, got %q", StatusConnected, statuses[0].Status)
	}
}

// --------------------------------------------------------------------------
// /api/endpoints/<idx>/connect — trigger connection
// --------------------------------------------------------------------------

func TestAPIEndpoints_Connect_Success(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: makeConnectFuncOK("a")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/0/connect", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	var s endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Status != StatusConnected {
		t.Errorf("want status %q, got %q", StatusConnected, s.Status)
	}
}

func TestAPIEndpoints_Connect_Failure(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: makeConnectFuncFail("connection refused")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/0/connect", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want %d, got %d", http.StatusBadGateway, rec.Code)
	}

	var s endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Status != StatusError {
		t.Errorf("want status %q, got %q", StatusError, s.Status)
	}
	if s.Error == "" {
		t.Error("want non-empty error message")
	}
}

func TestAPIEndpoints_Connect_InvalidIndex(t *testing.T) {
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: makeConnectFuncOK("a")},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/999/connect", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestAPIEndpoints_Connect_WrongMethod(t *testing.T) {
	h := MultiEndpointHandler([]EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: makeConnectFuncOK("a")},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/endpoints/0/connect", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// --------------------------------------------------------------------------
// /api/endpoints/<idx>/refresh — refresh connection
// --------------------------------------------------------------------------

func TestAPIEndpoints_Refresh_Success(t *testing.T) {
	callCount := 0
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: func() (http.Handler, error) {
			callCount++
			return makeEcho(fmt.Sprintf("version-%d", callCount)), nil
		}},
	}
	h := MultiEndpointHandler(endpoints)

	// Initial connect
	req := httptest.NewRequest(http.MethodPost, "/api/endpoints/0/connect", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if callCount != 1 {
		t.Fatalf("connect: want callCount 1, got %d", callCount)
	}

	// Refresh
	req = httptest.NewRequest(http.MethodPost, "/api/endpoints/0/refresh", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if callCount != 2 {
		t.Fatalf("refresh: want callCount 2, got %d", callCount)
	}

	var s endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Status != StatusConnected {
		t.Errorf("want status %q after refresh, got %q", StatusConnected, s.Status)
	}
}

// --------------------------------------------------------------------------
// Lazy connect — first access triggers connection
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_LazyConnectOnFirstAccess(t *testing.T) {
	connected := false
	endpoints := []EndpointInfo{
		{Target: "host-a:9080", ConnectFunc: func() (http.Handler, error) {
			connected = true
			return makeEcho("lazy-response"), nil
		}},
	}
	h := MultiEndpointHandler(endpoints)

	// Check initial status — should be pending
	req := httptest.NewRequest(http.MethodGet, "/api/endpoints", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var statuses []endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &statuses)
	if statuses[0].Status != StatusPending {
		t.Errorf("initial: want %q, got %q", StatusPending, statuses[0].Status)
	}
	if connected {
		t.Error("ConnectFunc should not be called before first endpoint access")
	}

	// Access the endpoint — should trigger lazy connect
	req = httptest.NewRequest(http.MethodGet, "/endpoint/0/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !connected {
		t.Error("ConnectFunc should have been called on first endpoint access")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "lazy-response" {
		t.Errorf("want 'lazy-response', got %q", rec.Body.String())
	}
}

// --------------------------------------------------------------------------
// Error status on failed connect
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_FailedConnect_ReturnsError(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "unreachable:9080", ConnectFunc: makeConnectFuncFail("connection refused")},
	}
	h := MultiEndpointHandler(endpoints)

	// Access the endpoint — should get error
	req := httptest.NewRequest(http.MethodGet, "/endpoint/0/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want %d, got %d", http.StatusBadGateway, rec.Code)
	}

	// Status should be error
	req = httptest.NewRequest(http.MethodGet, "/api/endpoints", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var statuses []endpointJSON
	json.Unmarshal(rec.Body.Bytes(), &statuses)
	if statuses[0].Status != StatusError {
		t.Errorf("want status %q, got %q", StatusError, statuses[0].Status)
	}
	if statuses[0].Error == "" {
		t.Error("want non-empty error message")
	}
}

// --------------------------------------------------------------------------
// Selector page shows status badges
// --------------------------------------------------------------------------

func TestMultiEndpointHandler_SelectorPage_ShowsStatusBadges(t *testing.T) {
	endpoints := []EndpointInfo{
		{Target: "localhost:9080", ConnectFunc: makeConnectFuncOK("svc0")},
		{Target: "localhost:9081", ConnectFunc: makeConnectFuncOK("svc1")},
	}
	h := MultiEndpointHandler(endpoints)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	// All endpoints should show pending status initially
	if !strings.Contains(body, "status-pending") {
		t.Error("selector page missing pending status badges")
	}
	// Should have connect buttons
	if !strings.Contains(body, "Connect") {
		t.Error("selector page missing Connect buttons")
	}
}
