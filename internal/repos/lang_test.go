package repos

import "testing"

func TestIsTestPath(t *testing.T) {
	for _, p := range []string{
		"test/helpers.go", "tests/helpers.go", "src/test/Foo.java",
		"src/main_test.go", "web/app.spec.ts", "web/app.test.ts",
		"internal/testdata/x.json", "web/__tests__/app.js",
		"src/mocks/store.go", "vendor/lib/x.go", "web/node_modules/x/index.js",
		// Anchoring the markers at a leading slash makes a directory at the
		// repository root read like the same directory nested below one, which
		// the old form spelled out for test/ and tests/ only.
		"mock/store.go", "fixtures/order.json",
	} {
		if !IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"src/shippingservice/main.go", "internal/latest/x.go",
		"src/contest/handler.go", "cmd/server/main.go",
	} {
		if IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = true, want false", p)
		}
	}
}

func TestIsGeneratedPath(t *testing.T) {
	// The paths the benchmark corpus actually holds, plus the same generator's
	// output for the other languages in it.
	for _, p := range []string{
		"src/frontend/genproto/demo.pb.go",
		"src/checkoutservice/genproto/demo_grpc.pb.go",
		"src/emailservice/demo_pb2.py",
		"src/recommendationservice/demo_pb2_grpc.py",
		"protos/demo.proto",
		"src/adservice/src/main/proto/demo.proto",
		"build/generated/Api.java",
		"src/proto/demo.pb.cc",
		"web/proto/demo_pb.js",
		"web/proto/demo_pb.d.ts",
	} {
		if !IsGeneratedPath(p) {
			t.Errorf("IsGeneratedPath(%q) = false, want true", p)
		}
	}
	// The implementation the questions are about, and paths that only look
	// like the markers.
	for _, p := range []string{
		"src/shippingservice/main.go",
		"src/productcatalogservice/product_catalog.go",
		"internal/protocol/reader.go",
		"src/prototype.go",
	} {
		if IsGeneratedPath(p) {
			t.Errorf("IsGeneratedPath(%q) = true, want false", p)
		}
	}
}

// The markers are exported so the SQL that ranks symbol lookups can be built
// from the same list the Go judgment uses; a copy protects that list from a
// caller that appends to what it is handed.
func TestPathMarkersAreCopies(t *testing.T) {
	for _, get := range []func() []string{TestPathMarkers, GeneratedPathMarkers} {
		first := get()
		if len(first) == 0 {
			t.Fatal("marker list is empty")
		}
		first[0] = "/mutated/"
		if get()[0] == "/mutated/" {
			t.Error("mutating the returned slice changed the shared marker list")
		}
	}
}
