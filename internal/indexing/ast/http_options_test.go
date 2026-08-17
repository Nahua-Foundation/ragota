package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// The request described by an options object rather than by argument
// positions. n8n makes thousands of calls this way; axios, got and
// System.Net.Http offer the same spelling.

func TestHTTPOptionsObjectTarget(t *testing.T) {
	tests := []struct {
		name   string
		lang   string
		file   string
		src    string
		key    string // http_call edge key
		host   string
		absent bool
	}{
		{
			name: "url and method in the options object, host from baseURL",
			lang: "typescript", file: "GenericFunctions.ts",
			src: `export async function upload(this: IExecuteFunctions) {
  return await this.helpers.httpRequest({
    method: 'POST',
    url: '/b/my-bucket/o',
    baseURL: 'https://storage.googleapis.com/upload/storage/v1',
    json: true,
  });
}
`,
			key:  "http:POST /upload/storage/v1/b/my-bucket/o",
			host: "storage.googleapis.com",
		},
		{
			name: "axios called with a single options object",
			lang: "typescript", file: "client.ts",
			src: `async function load() {
  return axios({ url: 'http://users/api/users', method: 'GET' });
}
`,
			key:  "http:GET /api/users",
			host: "users",
		},
		{
			name: "uri spelling on a helper receiver",
			lang: "typescript", file: "GenericFunctions.ts",
			src: `async function search(this: IExecuteFunctions) {
  return await this.helpers.request({ method: 'PUT', uri: 'https://api.hunter.io/v2/leads' });
}
`,
			key:  "http:PUT /v2/leads",
			host: "api.hunter.io",
		},
		{
			name: "options built into a variable first",
			lang: "typescript", file: "GenericFunctions.ts",
			src: `async function search(this: IExecuteFunctions) {
  const options = { method: 'DELETE', uri: 'https://api.hunter.io/v2/leads/1' };
  return await this.helpers.request(options);
}
`,
			key:  "http:DELETE /v2/leads/1",
			host: "api.hunter.io",
		},
		{
			name: "positional url still wins where the library uses one",
			lang: "typescript", file: "client.ts",
			src: `async function load() {
  await fetch('http://users/api/users/1', { method: 'PATCH' });
}
`,
			key:  "http:PATCH /api/users/1",
			host: "users",
		},
		{
			name: "csharp request message with the method first",
			lang: "csharp", file: "Caller.cs",
			src: `namespace Acme;

public class Caller
{
    public void Send()
    {
        var request = new HttpRequestMessage(HttpMethod.Post, "http://billing/api/billing/charge");
    }
}
`,
			key:  "http:POST /api/billing/charge",
			host: "billing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, tt.lang, tt.file, tt.src)
			e := findEdge(edges, storage.EdgeHTTPCall, tt.key)
			if e == nil {
				t.Fatalf("missing http_call %q; edges: %+v", tt.key, edgeNames(edges))
			}
			if meta := storage.DecodeEdgeMeta(e.Meta); meta.Host != tt.host {
				t.Errorf("host = %q, want %q", meta.Host, tt.host)
			}
		})
	}
}

// An options object without a target is not an HTTP call, however much it
// looks like one.
func TestHTTPOptionsObjectWithoutURL(t *testing.T) {
	src := `async function run() {
  await this.helpers.httpRequest({ method: 'POST', body: payload });
}
`
	_, edges := parseOrFail(t, "typescript", "run.ts", src)
	for _, e := range edges {
		if e.Kind == storage.EdgeHTTPCall {
			t.Errorf("unexpected http_call %q", e.DstName)
		}
	}
}

func TestURLFromFields(t *testing.T) {
	rule := &httpClientRule{URLFromFields: true, Conf: 0.8}
	res := func(expr string) (string, bool) { return unquote(expr) }

	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{name: "url", fields: map[string]string{"url": `'/a/b'`}, want: "/a/b"},
		{name: "uri", fields: map[string]string{"uri": `'/a/b'`}, want: "/a/b"},
		{name: "endpoint", fields: map[string]string{"endpoint": `'/a/b'`}, want: "/a/b"},
		{
			name:   "url joined onto baseURL",
			fields: map[string]string{"url": `'/b/x'`, "baseURL": `'https://s/v1'`},
			want:   "https://s/v1/b/x",
		},
		{
			name:   "baseURL alone is the target",
			fields: map[string]string{"baseUrl": `'https://s/v1/x'`},
			want:   "https://s/v1/x",
		},
		{
			name:   "an absolute url ignores the base",
			fields: map[string]string{"url": `'https://other/x'`, "baseURL": `'https://s/v1'`},
			want:   "https://other/x",
		},
		{name: "no target field", fields: map[string]string{"method": `'GET'`}},
		{name: "target is not a URL", fields: map[string]string{"url": `'orders'`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _, ok := rule.urlFromFields(tt.fields, res)
			if ok != (tt.want != "") {
				t.Fatalf("ok = %v, want %v", ok, tt.want != "")
			}
			if got != tt.want {
				t.Errorf("url = %q, want %q", got, tt.want)
			}
		})
	}
}
