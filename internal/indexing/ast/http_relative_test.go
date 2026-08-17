package ast

import (
	"testing"

	"github.com/Nahua-Foundation/ragota/internal/storage"
)

// Requests written against a client's base address. A .NET service configures
// its HttpClient with a BaseAddress and then writes "api/orders", so the
// leading "/" that marks every other language's request target is not there —
// eShop spells 34 of its 55 outbound calls that way, and the route it reaches
// is the same one the server declares as "/api/orders".

func TestCSharpRelativeRequestTargets(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		src    string
		keys   []string // http_call edge keys the file must produce
		absent []string
	}{
		{
			name: "relative literal reaches the same route as the rooted spelling",
			file: "OrderingApiTests.cs",
			src: `public class OrderingApiTests
{
    private readonly HttpClient _httpClient;

    public async Task GetOrders()
    {
        var response = await _httpClient.GetAsync("api/orders");
        var one = await _httpClient.GetAsync("/api/orders");
        await _httpClient.PutAsync("api/orders/cancel", content);
    }
}
`,
			keys: []string{"http:GET /api/orders", "http:PUT /api/orders/cancel"},
		},
		{
			name: "generic json helper is the same call as its non-generic overload",
			file: "WebHooksClient.cs",
			src: `public class WebhooksClient
{
    public Task<IEnumerable<WebhookResponse>> LoadWebhooks()
        => client.GetFromJsonAsync<IEnumerable<Models.WebhookResponse>>("/api/webhooks");
}
`,
			keys: []string{"http:GET /api/webhooks"},
		},
		{
			name: "target assembled into a local, on a relative base field",
			file: "CatalogService.cs",
			src: `public class CatalogService(HttpClient httpClient) : ICatalogService
{
    private readonly string remoteServiceBaseUrl = "api/catalog/";

    public Task<CatalogItem?> GetCatalogItem(int id)
    {
        var uri = $"{remoteServiceBaseUrl}items/{id}?api-version=2.0";
        return httpClient.GetFromJsonAsync<CatalogItem>(uri);
    }

    public async Task<IEnumerable<CatalogBrand>> GetBrands()
    {
        var uri = $"{remoteServiceBaseUrl}catalogBrands";
        return await httpClient.GetFromJsonAsync<CatalogBrand[]>(uri);
    }
}
`,
			// Each method's own uri: a file-scoped value would answer both call
			// sites with whichever assignment came last.
			keys:   []string{"http:GET /api/catalog/items/{}", "http:GET /api/catalog/catalogBrands"},
			absent: []string{"http:GET /api/catalog/items"},
		},
		{
			name: "concatenated target keeps its literal first segment",
			file: "UserControllerTests.cs",
			src: `public class UserControllerTests
{
    private Task<HttpResponseMessage> UpdatePassword(HttpClient httpClient, Guid userId)
        => httpClient.PostAsJsonAsync("Users/" + userId.ToString("N", CultureInfo.InvariantCulture) + "/Password", request);
}
`,
			keys:   []string{"http:POST /Users/{}N{}/Password"},
			absent: []string{"http:POST /{}N{}/Password"},
		},
		{
			name: "request object built with a relative resource",
			file: "OrderingService.cs",
			src: `public class OrderingService(HttpClient httpClient)
{
    public Task CreateOrder(CreateOrderRequest request)
    {
        var requestMessage = new HttpRequestMessage(HttpMethod.Post, "api/orders");
        return httpClient.SendAsync(requestMessage);
    }
}
`,
			keys: []string{"http:POST /api/orders"},
		},
		{
			name: "a single segment is not a route",
			file: "CacheService.cs",
			src: `public class CacheService
{
    public Task<byte[]> Read(IDistributedCache cacheClient)
        => cacheClient.GetAsync("basket");
}
`,
			absent: []string{"http:GET /basket"},
		},
		{
			name: "a target that is entirely runtime values names no route",
			file: "GrantUrlTesterService.cs",
			src: `class GrantUrlTesterService(IHttpClientFactory factory)
{
    public async Task<bool> TestGrantUrl(string url)
    {
        var client = factory.CreateClient();
        var msg = new HttpRequestMessage(HttpMethod.Options, url);
        return (await client.SendAsync(msg)).IsSuccessStatusCode;
    }
}
`,
			absent: []string{"http:OPTIONS /url", "http:GET /url"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, edges := parseOrFail(t, "csharp", tt.file, tt.src)
			for _, want := range tt.keys {
				if findEdge(edges, storage.EdgeHTTPCall, want) == nil {
					t.Errorf("http_call %q missing from %v", want, edgeNamesOfKind(edges, storage.EdgeHTTPCall))
				}
			}
			for _, not := range tt.absent {
				if findEdge(edges, storage.EdgeHTTPCall, not) != nil {
					t.Errorf("http_call %q was emitted and should not be", not)
				}
			}
		})
	}
}

// The shape gate for a target with no leading "/" and no scheme left to
// recognize it by. Everything it admits becomes a route key, so it is the only
// thing standing between a cache key and a phantom contract.
func TestIsRelativeRoute(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"api/orders", true},
		{"api/catalog/items/1/pic", true},
		{"api/catalog/items?name=Alpine&PageSize=5", true},
		{"api/catalog/items/by/Wanderer%20Black%20Hiking%20Boots", true},
		{"api/catalog/items/{}", true},
		{"Users/{}/PlayedItems/{}", true},
		{"items", false},       // one segment: as likely a cache key
		{"/api/orders", false}, // already rooted; isURLShaped's business
		{"https://x/y", false}, // absolute; isURLShaped's business
		{"2.0/items", false},   // opens on a version, not a path
		{"localhost:9092/x", false},
		{"../testdata/file.json", false},
		{"application/json; charset=utf-8", false},
		{"key.deserializer", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isRelativeRoute(tt.in); got != tt.want {
			t.Errorf("isRelativeRoute(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The rule is opt-in: a client whose argument could be something other than a
// request target does not get the relative form.
func TestRelativeTargetIsPerRule(t *testing.T) {
	res := func(expr string) (string, bool) { return unquote(expr) }
	if _, _, ok := resolveRequestURL(`"api/orders"`, res, 0.9, false); ok {
		t.Error("a rule that did not declare RelativeURL resolved a relative target")
	}
	if _, _, ok := resolveRequestURL(`"api/orders"`, res, 0.9, true); !ok {
		t.Error("a rule that declared RelativeURL did not resolve a relative target")
	}
}
