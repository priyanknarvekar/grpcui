package standalone

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// EndpointInfo pairs a human-readable target label with the HTTP handler that
// serves the gRPC UI for that endpoint.
type EndpointInfo struct {
	// Target is the host:port (or display name) of the gRPC server. It is
	// shown in the endpoint selector and in the page heading.
	Target string
	// Handler is the standalone Handler (or any http.Handler) that serves the
	// full gRPC UI for this endpoint.
	Handler http.Handler
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
// The returned handler mounts each endpoint's Handler under the sub-path
// "/endpoint/<idx>/". A selector page at "/" lets the user pick an endpoint
// and is redirected to the corresponding sub-path. A POST to "/switch" with
// form field "ep" (an integer index) performs the same redirect.
//
// If only one endpoint is provided the handler still works correctly, but
// callers may prefer to use the single-endpoint Handler directly.
func MultiEndpointHandler(endpoints []EndpointInfo, opts ...MultiEndpointOption) http.Handler {
	mopts := &multiHandlerOptions{}
	for _, o := range opts {
		o.applyMulti(mopts)
	}
	title := mopts.title
	if title == "" {
		title = "gRPC Web UI"
	}
	mux := http.NewServeMux()

	// Mount each endpoint handler under /endpoint/<idx>/
	for i, ep := range endpoints {
		prefix := fmt.Sprintf("/endpoint/%d", i)
		ep := ep // capture loop var
		mux.Handle(prefix+"/", http.StripPrefix(prefix, ep.Handler))
		// Redirect /endpoint/<idx> (no trailing slash) → /endpoint/<idx>/
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
		})
	}

	// /switch?ep=<idx>  or  POST /switch  with form field ep=<idx>
	mux.HandleFunc("/switch", func(w http.ResponseWriter, r *http.Request) {
		epStr := r.FormValue("ep")
		idx, err := strconv.Atoi(epStr)
		if err != nil || idx < 0 || idx >= len(endpoints) {
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
		if len(endpoints) == 1 {
			http.Redirect(w, r, "/endpoint/0/", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var buf bytes.Buffer
		if err := selectorTemplate.Execute(&buf, selectorTemplateData{
			Title:     title,
			Endpoints: endpoints,
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
// values with nil Handlers. Callers must set Handler before passing the result
// to MultiEndpointHandler. This helper is primarily intended for testing and
// CLI use.
func ParseTargets(targets []string) []EndpointInfo {
	endpoints := make([]EndpointInfo, len(targets))
	for i, t := range targets {
		endpoints[i] = EndpointInfo{Target: t}
	}
	return endpoints
}

// --- selector page template ---------------------------------------------------

type selectorTemplateData struct {
	Title     string
	Endpoints []EndpointInfo
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
      max-width: 520px;
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
      cursor: pointer;
      text-decoration: none;
      color: inherit;
      transition: background 0.18s ease, border-color 0.18s ease, transform 0.12s ease;
    }
    .endpoint-item:hover {
      background: rgba(0,188,212,0.18);
      border-color: rgba(0,188,212,0.45);
      transform: translateY(-1px);
    }
    .endpoint-item:active {
      transform: translateY(0);
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
    }
    .endpoint-arrow {
      margin-left: auto;
      color: rgba(255,255,255,0.3);
      font-size: 16px;
      transition: color 0.18s;
    }
    .endpoint-item:hover .endpoint-arrow {
      color: #00e5ff;
    }

    .footer {
      margin-top: 8px;
      font-size: 12px;
      color: rgba(255,255,255,0.35);
      text-align: center;
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="logo-row">
      <div class="logo-badge">gRPC</div>
      <div>
        <div class="logo-title">{{.Title}}</div>
        <div class="logo-subtitle">Multi-endpoint selector</div>
      </div>
    </div>

    <h1>Choose an endpoint</h1>

    <ul class="endpoint-list">
      {{range $i, $ep := .Endpoints}}
      <li>
        <a class="endpoint-item" href="/endpoint/{{$i}}/" id="ep-{{$i}}">
          <span class="endpoint-idx">{{$i}}</span>
          <span class="endpoint-host">{{$ep.Target}}</span>
          <span class="endpoint-arrow">&#8594;</span>
        </a>
      </li>
      {{end}}
    </ul>

    <div class="footer">{{len .Endpoints}} endpoint{{if gt (len .Endpoints) 1}}s{{end}} configured</div>
  </div>
</body>
</html>
`
