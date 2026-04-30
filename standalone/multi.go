package standalone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// EndpointStatus represents the connection state of a lazy endpoint.
type EndpointStatus string

const (
	StatusPending    EndpointStatus = "pending"
	StatusConnecting EndpointStatus = "connecting"
	StatusConnected  EndpointStatus = "connected"
	StatusError      EndpointStatus = "error"
)

// EndpointInfo pairs a human-readable target label with a lazy connect
// function. The gRPC connection and reflection are performed on demand,
// when the user selects the endpoint from the UI.
type EndpointInfo struct {
	// Target is the host:port (or display name) of the gRPC server. It is
	// shown in the endpoint selector and in the page heading.
	Target string

	// Handler is the standalone Handler (or any http.Handler) that serves the
	// full gRPC UI for this endpoint. It is nil until ConnectFunc succeeds.
	Handler http.Handler

	// ConnectFunc is called lazily to dial the gRPC endpoint, perform
	// reflection, and build the HTTP handler. If nil, Handler must be set
	// at construction time (eager mode for backward compatibility).
	ConnectFunc func() (http.Handler, error)
}

// lazyEndpoint wraps an EndpointInfo with mutex-protected state for
// lazy connection management.
type lazyEndpoint struct {
	mu     sync.Mutex
	info   EndpointInfo
	status EndpointStatus
	err    string
}

// endpointJSON is the JSON representation sent by /api/endpoints.
type endpointJSON struct {
	Index  int            `json:"index"`
	Target string         `json:"target"`
	Status EndpointStatus `json:"status"`
	Error  string         `json:"error,omitempty"`
}

func newLazyEndpoint(info EndpointInfo) *lazyEndpoint {
	ep := &lazyEndpoint{
		info: info,
	}
	if info.Handler != nil {
		ep.status = StatusConnected
	} else {
		ep.status = StatusPending
	}
	return ep
}

// connect attempts to connect the endpoint. Returns true if the endpoint
// is now connected (either freshly or already was).
func (ep *lazyEndpoint) connect() error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.info.ConnectFunc == nil {
		if ep.info.Handler != nil {
			ep.status = StatusConnected
			return nil
		}
		ep.status = StatusError
		ep.err = "no connect function configured"
		return fmt.Errorf("no connect function configured")
	}

	ep.status = StatusConnecting
	ep.err = ""

	// Unlock during the potentially slow dial+reflect so we don't block
	// status reads. We use a local flag to detect concurrent calls.
	ep.mu.Unlock()

	handler, err := ep.info.ConnectFunc()

	ep.mu.Lock()
	// mu is re-locked; defer will unlock

	if err != nil {
		ep.status = StatusError
		ep.err = err.Error()
		return err
	}

	ep.info.Handler = handler
	ep.status = StatusConnected
	ep.err = ""
	return nil
}

// refresh re-runs the connect function to reload reflection definitions.
func (ep *lazyEndpoint) refresh() error {
	return ep.connect()
}

// getStatus returns the current status snapshot.
func (ep *lazyEndpoint) getStatus() endpointJSON {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return endpointJSON{
		Target: ep.info.Target,
		Status: ep.status,
		Error:  ep.err,
	}
}

// getHandler returns the handler if connected, or nil.
func (ep *lazyEndpoint) getHandler() http.Handler {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.info.Handler
}

// MultiEndpointOption configures the behaviour of MultiEndpointHandler.
type MultiEndpointOption interface {
	applyMulti(opts *multiHandlerOptions)
}

type multiHandlerOptions struct {
	title string
}

type multiOptFunc func(opts *multiHandlerOptions)

func (f multiOptFunc) applyMulti(opts *multiHandlerOptions) { f(opts) }

// WithMultiTitle sets the application title shown on the endpoint-selector
// page. When not set the page defaults to "gRPC Web UI".
func WithMultiTitle(title string) MultiEndpointOption {
	return multiOptFunc(func(opts *multiHandlerOptions) {
		opts.title = title
	})
}

// MultiEndpointHandler returns an http.Handler that allows switching between
// multiple gRPC endpoints from a selector page.
//
// Endpoints are connected lazily — no gRPC dial or reflection happens until
// the user selects an endpoint from the catalog. This allows the web server
// to start even when some or all endpoints are unreachable.
//
// The returned handler provides:
//   - "/" — selector page (or instant redirect for single-endpoint)
//   - "/endpoint/<idx>/" — serves the gRPC UI for that endpoint (lazy-connects first)
//   - "/api/endpoints" — JSON status of all endpoints
//   - "/api/endpoints/<idx>/connect" — POST to trigger connection
//   - "/api/endpoints/<idx>/refresh" — POST to refresh reflection
//   - "/switch" — redirect by ep index
func MultiEndpointHandler(endpoints []EndpointInfo, opts ...MultiEndpointOption) http.Handler {
	mopts := &multiHandlerOptions{}
	for _, o := range opts {
		o.applyMulti(mopts)
	}
	title := mopts.title
	if title == "" {
		title = "gRPC Web UI"
	}

	// Wrap each endpoint in a lazy state wrapper.
	lazy := make([]*lazyEndpoint, len(endpoints))
	for i, ep := range endpoints {
		lazy[i] = newLazyEndpoint(ep)
	}

	mux := http.NewServeMux()

	// Mount each endpoint handler under /endpoint/<idx>/
	for i := range lazy {
		idx := i
		prefix := fmt.Sprintf("/endpoint/%d", idx)

		mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			ep := lazy[idx]

			// Lazy-connect on first access.
			handler := ep.getHandler()
			if handler == nil {
				if err := ep.connect(); err != nil {
					http.Error(w, fmt.Sprintf("Failed to connect to %s: %s", ep.info.Target, err.Error()), http.StatusBadGateway)
					return
				}
				handler = ep.getHandler()
			}
			if handler == nil {
				http.Error(w, "endpoint not connected", http.StatusServiceUnavailable)
				return
			}

			// Strip the prefix so sub-handlers see paths starting at "/".
			http.StripPrefix(prefix, handler).ServeHTTP(w, r)
		})

		// Redirect /endpoint/<idx> (no trailing slash) → /endpoint/<idx>/
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == prefix {
				http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
				return
			}
		})
	}

	// /api/endpoints — JSON status of all endpoints
	mux.HandleFunc("/api/endpoints", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		statuses := make([]endpointJSON, len(lazy))
		for i, ep := range lazy {
			s := ep.getStatus()
			s.Index = i
			statuses[i] = s
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(statuses)
	})

	// /api/endpoints/<idx>/connect — POST to trigger (re)connection
	mux.HandleFunc("/api/endpoints/", func(w http.ResponseWriter, r *http.Request) {
		// Parse: /api/endpoints/<idx>/connect or /api/endpoints/<idx>/refresh
		path := strings.TrimPrefix(r.URL.Path, "/api/endpoints/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}

		idx, err := strconv.Atoi(parts[0])
		if err != nil || idx < 0 || idx >= len(lazy) {
			http.Error(w, fmt.Sprintf("invalid endpoint index %q", parts[0]), http.StatusBadRequest)
			return
		}

		action := parts[1]
		ep := lazy[idx]

		switch action {
		case "connect":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			connectErr := ep.connect()
			s := ep.getStatus()
			s.Index = idx
			w.Header().Set("Content-Type", "application/json")
			if connectErr != nil {
				w.WriteHeader(http.StatusBadGateway)
			}
			json.NewEncoder(w).Encode(s)

		case "refresh":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			refreshErr := ep.refresh()
			s := ep.getStatus()
			s.Index = idx
			w.Header().Set("Content-Type", "application/json")
			if refreshErr != nil {
				w.WriteHeader(http.StatusBadGateway)
			}
			json.NewEncoder(w).Encode(s)

		default:
			http.NotFound(w, r)
		}
	})

	// /switch?ep=<idx>  or  POST /switch  with form field ep=<idx>
	mux.HandleFunc("/switch", func(w http.ResponseWriter, r *http.Request) {
		epStr := r.FormValue("ep")
		idx, err := strconv.Atoi(epStr)
		if err != nil || idx < 0 || idx >= len(lazy) {
			http.Error(w, fmt.Sprintf("invalid endpoint index %q", epStr), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/endpoint/%d/", idx), http.StatusSeeOther)
	})

	// / → selector page (or instant redirect when there is only one endpoint)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if len(lazy) == 1 {
			http.Redirect(w, r, "/endpoint/0/", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Build template data with current statuses.
		epDatas := make([]selectorEndpoint, len(lazy))
		for i, ep := range lazy {
			s := ep.getStatus()
			epDatas[i] = selectorEndpoint{Target: s.Target, Status: s.Status, Error: s.Error}
		}

		var buf bytes.Buffer
		if err := selectorTemplate.Execute(&buf, selectorTemplateData{
			Title:     title,
			Endpoints: epDatas,
		}); err != nil {
			http.Error(w, "failed to render selector: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	})

	return mux
}

// ParseTargets converts a slice of "host:port" strings into EndpointInfo
// values with nil Handlers. Callers must set Handler or ConnectFunc before
// passing the result to MultiEndpointHandler. This helper is primarily
// intended for testing and CLI use.
func ParseTargets(targets []string) []EndpointInfo {
	endpoints := make([]EndpointInfo, len(targets))
	for i, t := range targets {
		endpoints[i] = EndpointInfo{Target: t}
	}
	return endpoints
}

// --- selector page template ---------------------------------------------------

type selectorEndpoint struct {
	Target string
	Status EndpointStatus
	Error  string
}

type selectorTemplateData struct {
	Title     string
	Endpoints []selectorEndpoint
}

var selectorTemplate = template.Must(template.New("selector").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"repeat": func(s string, n int) string {
		return strings.Repeat(s, n)
	},
}).Parse(selectorPageHTML))

const selectorPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{.Title}} — Select Endpoint</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }

    body {
      margin: 0;
      padding: 0;
      font-family: Roboto, "Helvetica Neue", Helvetica, Arial, sans-serif;
      font-size: 15px;
      line-height: 1.5;
      color: #e8f4f5;
      background: linear-gradient(135deg, #0a2e32 0%, #0c4348 50%, #0f5760 100%);
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
    }

    .card {
      background: rgba(255,255,255,0.07);
      border: 1px solid rgba(255,255,255,0.12);
      border-radius: 16px;
      padding: 48px 56px;
      width: 100%;
      max-width: 600px;
      box-shadow: 0 24px 64px rgba(0,0,0,0.45);
      backdrop-filter: blur(10px);
      -webkit-backdrop-filter: blur(10px);
    }

    .logo-row {
      display: flex;
      align-items: center;
      gap: 14px;
      margin-bottom: 32px;
    }
    .logo-badge {
      width: 46px;
      height: 46px;
      border-radius: 12px;
      background: linear-gradient(135deg, #00bcd4, #00838f);
      display: flex;
      align-items: center;
      justify-content: center;
      font-weight: 800;
      font-size: 18px;
      color: #fff;
      letter-spacing: -0.5px;
      flex-shrink: 0;
    }
    .logo-title {
      font-size: 22px;
      font-weight: 700;
      color: #fff;
    }
    .logo-subtitle {
      font-size: 12px;
      color: rgba(255,255,255,0.5);
      margin-top: 1px;
    }

    h1 {
      font-size: 17px;
      font-weight: 600;
      color: rgba(255,255,255,0.9);
      margin: 0 0 20px 0;
    }

    .endpoint-list {
      list-style: none;
      margin: 0 0 28px 0;
      padding: 0;
      display: flex;
      flex-direction: column;
      gap: 10px;
    }

    .endpoint-item {
      display: flex;
      align-items: center;
      gap: 12px;
      padding: 14px 18px;
      background: rgba(255,255,255,0.05);
      border: 1px solid rgba(255,255,255,0.09);
      border-radius: 10px;
      transition: background 0.18s ease, border-color 0.18s ease, transform 0.12s ease;
    }
    .endpoint-item:hover {
      background: rgba(0,188,212,0.10);
      border-color: rgba(0,188,212,0.30);
    }

    .endpoint-idx {
      flex-shrink: 0;
      width: 28px;
      height: 28px;
      border-radius: 8px;
      background: rgba(0,188,212,0.22);
      color: #00e5ff;
      font-size: 12px;
      font-weight: 700;
      display: flex;
      align-items: center;
      justify-content: center;
    }
    .endpoint-host {
      font-size: 14px;
      font-weight: 500;
      color: #e0f7fa;
      font-family: "SFMono-Regular", "Consolas", "Liberation Mono", Menlo, monospace;
      flex: 1;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    /* Status badges */
    .status-badge {
      flex-shrink: 0;
      width: 10px;
      height: 10px;
      border-radius: 50%;
      transition: background-color 0.3s ease;
    }
    .status-pending    { background-color: #546e7a; }
    .status-connecting { background-color: #ffb74d; animation: pulse 1s infinite; }
    .status-connected  { background-color: #66bb6a; }
    .status-error      { background-color: #ef5350; }

    @keyframes pulse {
      0%, 100% { opacity: 1; }
      50% { opacity: 0.4; }
    }

    .endpoint-actions {
      display: flex;
      gap: 6px;
      margin-left: auto;
      flex-shrink: 0;
    }

    .btn {
      border: none;
      border-radius: 6px;
      padding: 6px 14px;
      font-size: 12px;
      font-weight: 600;
      cursor: pointer;
      transition: all 0.18s ease;
      text-decoration: none;
      display: inline-flex;
      align-items: center;
      gap: 4px;
    }
    .btn-connect {
      background: linear-gradient(135deg, #00bcd4, #0097a7);
      color: #fff;
    }
    .btn-connect:hover { background: linear-gradient(135deg, #00e5ff, #00bcd4); }
    .btn-connect:disabled {
      background: rgba(0,188,212,0.3);
      cursor: not-allowed;
    }

    .btn-open {
      background: linear-gradient(135deg, #66bb6a, #43a047);
      color: #fff;
    }
    .btn-open:hover { background: linear-gradient(135deg, #81c784, #66bb6a); }

    .btn-refresh {
      background: rgba(255,255,255,0.08);
      color: #b2dfdb;
      border: 1px solid rgba(255,255,255,0.12);
    }
    .btn-refresh:hover { background: rgba(255,255,255,0.15); color: #e0f7fa; }
    .btn-refresh:disabled {
      opacity: 0.4;
      cursor: not-allowed;
    }

    .btn-retry {
      background: rgba(239,83,80,0.15);
      color: #ef9a9a;
      border: 1px solid rgba(239,83,80,0.3);
    }
    .btn-retry:hover { background: rgba(239,83,80,0.3); color: #fff; }

    .error-msg {
      font-size: 11px;
      color: #ef9a9a;
      margin-top: 6px;
      padding: 6px 10px;
      background: rgba(239,83,80,0.08);
      border-radius: 6px;
      border: 1px solid rgba(239,83,80,0.15);
      word-break: break-word;
      display: none;
    }
    .error-msg.visible { display: block; }

    .footer {
      margin-top: 8px;
      font-size: 12px;
      color: rgba(255,255,255,0.35);
      text-align: center;
    }

    /* Spinner for connecting state */
    .spinner {
      width: 14px;
      height: 14px;
      border: 2px solid rgba(255,255,255,0.3);
      border-top-color: #fff;
      border-radius: 50%;
      animation: spin 0.6s linear infinite;
      display: none;
    }
    .spinner.visible { display: inline-block; }

    @keyframes spin {
      to { transform: rotate(360deg); }
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="logo-row">
      <div class="logo-badge">gRPC</div>
      <div>
        <div class="logo-title">{{.Title}}</div>
        <div class="logo-subtitle">Endpoint catalog</div>
      </div>
    </div>

    <h1>Choose an endpoint</h1>

    <ul class="endpoint-list" id="endpoint-list">
      {{range $i, $ep := .Endpoints}}
      <li>
        <div class="endpoint-item" id="ep-item-{{$i}}">
          <span class="endpoint-idx">{{$i}}</span>
          <span class="status-badge status-{{$ep.Status}}" id="ep-status-{{$i}}"></span>
          <span class="endpoint-host">{{$ep.Target}}</span>
          <div class="endpoint-actions" id="ep-actions-{{$i}}">
            <span class="spinner" id="ep-spinner-{{$i}}"></span>
            <button class="btn btn-connect" id="ep-connect-{{$i}}" onclick="connectEndpoint({{$i}})">Connect</button>
          </div>
        </div>
        <div class="error-msg" id="ep-error-{{$i}}"></div>
      </li>
      {{end}}
    </ul>

    <div class="footer">{{len .Endpoints}} endpoint{{if gt (len .Endpoints) 1}}s{{end}} configured</div>
  </div>

  <script>
    var endpointCount = {{len .Endpoints}};

    function updateEndpointUI(ep) {
      var statusEl = document.getElementById('ep-status-' + ep.index);
      var actionsEl = document.getElementById('ep-actions-' + ep.index);
      var spinnerEl = document.getElementById('ep-spinner-' + ep.index);
      var errorEl = document.getElementById('ep-error-' + ep.index);

      // Update status badge
      statusEl.className = 'status-badge status-' + ep.status;

      // Update error message
      if (ep.status === 'error' && ep.error) {
        errorEl.textContent = ep.error;
        errorEl.classList.add('visible');
      } else {
        errorEl.textContent = '';
        errorEl.classList.remove('visible');
      }

      // Update action buttons based on status
      var buttons = '';
      switch(ep.status) {
        case 'pending':
          spinnerEl.classList.remove('visible');
          buttons = '<button class="btn btn-connect" onclick="connectEndpoint(' + ep.index + ')">Connect</button>';
          break;
        case 'connecting':
          spinnerEl.classList.add('visible');
          buttons = '<button class="btn btn-connect" disabled>Connecting…</button>';
          break;
        case 'connected':
          spinnerEl.classList.remove('visible');
          buttons = '<a class="btn btn-open" href="/endpoint/' + ep.index + '/">Open</a>' +
                    '<button class="btn btn-refresh" onclick="refreshEndpoint(' + ep.index + ')">↻ Refresh</button>';
          break;
        case 'error':
          spinnerEl.classList.remove('visible');
          buttons = '<button class="btn btn-retry" onclick="connectEndpoint(' + ep.index + ')">Retry</button>';
          break;
      }
      // Keep spinner, replace the rest
      actionsEl.innerHTML = '<span class="spinner' + (ep.status === 'connecting' ? ' visible' : '') + '" id="ep-spinner-' + ep.index + '"></span>' + buttons;
    }

    function connectEndpoint(idx) {
      // Optimistic UI: show connecting state
      updateEndpointUI({index: idx, status: 'connecting', error: ''});

      fetch('/api/endpoints/' + idx + '/connect', {method: 'POST'})
        .then(function(resp) { return resp.json(); })
        .then(function(data) {
          data.index = idx;
          updateEndpointUI(data);
          if (data.status === 'connected') {
            // Brief delay so the user sees the green status, then navigate
            setTimeout(function() {
              window.location.href = '/endpoint/' + idx + '/';
            }, 400);
          }
        })
        .catch(function(err) {
          updateEndpointUI({index: idx, status: 'error', error: 'Network error: ' + err.message});
        });
    }

    function refreshEndpoint(idx) {
      updateEndpointUI({index: idx, status: 'connecting', error: ''});

      fetch('/api/endpoints/' + idx + '/refresh', {method: 'POST'})
        .then(function(resp) { return resp.json(); })
        .then(function(data) {
          data.index = idx;
          updateEndpointUI(data);
        })
        .catch(function(err) {
          updateEndpointUI({index: idx, status: 'error', error: 'Network error: ' + err.message});
        });
    }

    // Poll endpoint statuses periodically to keep the UI in sync
    function pollStatuses() {
      fetch('/api/endpoints')
        .then(function(resp) { return resp.json(); })
        .then(function(statuses) {
          statuses.forEach(function(ep) { updateEndpointUI(ep); });
        })
        .catch(function() {});
    }

    // Poll every 5 seconds
    setInterval(pollStatuses, 5000);
  </script>
</body>
</html>
`
