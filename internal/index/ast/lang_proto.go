package ast

import (
	"regexp"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/domain"
	"github.com/Nahua-Foundation/ragota/internal/store"
)

// protoParser parses .proto files into contract units:
// proto_service, rpc_method, proto_message, proto_field.
//
// It is a lightweight hand-rolled parser sufficient for the proto2/proto3
// subset that defines services, rpcs, messages and scalar/message fields.
type protoParser struct{}

// newProtoParser creates a proto contract parser.
func newProtoParser() *protoParser { return &protoParser{} }

// Language returns the language name.
func (p *protoParser) Language() string { return "proto" }

// Parse extracts contract units and edges from a .proto file.
func (p *protoParser) Parse(filePath, content string) ([]*domain.ASTUnit, []*domain.Edge, error) {
	fc := &fileCtx{path: filePath, src: []byte(content)}

	pkg := ""
	lines := strings.Split(content, "\n")

	// depth is the running brace nesting level. Each scope remembers the depth
	// its block opened at, so a scope is popped exactly when nesting falls back
	// below it — whether the block was closed on its own line ("message Empty {}"),
	// by a later "}", or after an anonymous option block that opened braces of
	// its own.
	type scope struct {
		kind  string // "service" | "message" | "enum" | "rpc"
		name  string
		idx   int // unit index
		depth int
	}
	var stack []scope
	depth := 0

	qualifiedMsg := func(name string) string {
		if pkg != "" {
			return "proto:" + pkg + "." + name
		}
		return "proto:" + name
	}

	addUnit := func(lineNo int, kind, name, qualified, signature string) int {
		u := &domain.ASTUnit{
			Kind:      kind,
			Name:      name,
			Qualified: qualified,
			Signature: signature,
			StartLine: lineNo + 1,
			EndLine:   lineNo + 1,
			Hash:      hashString(qualified + signature),
		}
		fc.units = append(fc.units, u)
		return len(fc.units) - 1
	}

	// httpOpt tracks an open `option (google.api.http) = { ... }` block and the
	// rpc it annotates, so grpc-gateway REST bindings become http_route units.
	httpOptRPC, httpOptDepth := -1, 0

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.Contains(line, "google.api.http") || strings.Contains(line, "additional_bindings") {
			httpOptRPC, httpOptDepth = -1, depth
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k].kind == "rpc" {
					httpOptRPC = stack[k].idx
					break
				}
			}
		}
		if httpOptRPC >= 0 {
			addGatewayRoutes(fc, line, i, httpOptRPC)
		}

		// push records a scope opened on this line at the nesting level its
		// block occupies (one deeper than the level the line started at).
		push := func(s scope) {
			s.depth = depth + 1
			stack = append(stack, s)
		}

		func() {
			switch {
			case strings.HasPrefix(line, "package "):
				pkg = strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "package ")), ";")

			case strings.HasPrefix(line, "service "):
				name := identAfter(line, "service ")
				if name != "" {
					q := "grpc:" + prefixed(pkg, name)
					idx := addUnit(i, store.KindProtoService, name, q, "")
					push(scope{kind: "service", name: name, idx: idx})
				}

			case strings.HasPrefix(line, "message "):
				name := identAfter(line, "message ")
				if name != "" {
					full := name
					if len(stack) > 0 && stack[len(stack)-1].kind == "message" {
						full = stack[len(stack)-1].name + "." + name
					}
					idx := addUnit(i, store.KindProtoMessage, name, qualifiedMsg(full), "")
					push(scope{kind: "message", name: full, idx: idx})
				}

			case strings.HasPrefix(line, "enum "):
				if name := identAfter(line, "enum "); name != "" {
					push(scope{kind: "enum", name: name, idx: -1})
				}

			case strings.HasPrefix(line, "rpc "):
				if len(stack) == 0 || stack[len(stack)-1].kind != "service" {
					return
				}
				svc := stack[len(stack)-1]
				name, req, resp := parseRPC(line)
				if name == "" {
					return
				}
				q := "grpc:" + prefixed(pkg, svc.name) + "/" + name
				sig := "rpc " + name + "(" + req + ") returns (" + resp + ")"
				idx := addUnit(i, store.KindRPCMethod, name, q, sig)
				if req != "" {
					fc.addEdge(idx, store.EdgeRPCRequest, qualifiedMsg(req), i+1, contract.ConfExact, nil)
				}
				if resp != "" {
					fc.addEdge(idx, store.EdgeRPCResponse, qualifiedMsg(resp), i+1, contract.ConfExact, nil)
				}
				// rpc lines may open a block for options
				if strings.Contains(line, "{") {
					push(scope{kind: "rpc", name: name, idx: idx})
				}

			case strings.HasPrefix(line, "}"):
				// Handled by the brace balance below.

			default:
				// Field inside a message: "string user_id = 1;" / "repeated Item items = 2;"
				if len(stack) == 0 || stack[len(stack)-1].kind != "message" {
					return
				}
				msg := stack[len(stack)-1]
				fieldType, fieldName, ok := parseField(line)
				if !ok {
					return
				}
				q := qualifiedMsg(msg.name) + "." + fieldName
				addUnit(i, store.KindProtoField, fieldName, q, fieldType)
			}
		}()

		// Balance the braces on this line and close every scope whose block
		// ended, so single-line blocks ("message Empty {}") and anonymous
		// option blocks leave the stack correct.
		opens, closes := strings.Count(line, "{"), strings.Count(line, "}")
		if opens == 0 && closes == 0 {
			continue
		}
		depth += opens - closes
		for len(stack) > 0 && depth < stack[len(stack)-1].depth {
			if top := stack[len(stack)-1]; top.idx >= 0 {
				fc.units[top.idx].EndLine = i + 1
			}
			stack = stack[:len(stack)-1]
		}
		if httpOptRPC >= 0 && depth <= httpOptDepth {
			httpOptRPC = -1
		}
	}

	return fc.units, fc.edges, nil
}

// reGatewayBinding matches one grpc-gateway HTTP binding inside an
// `option (google.api.http)` block: `get: "/v1/orders/{order_id}"`.
var reGatewayBinding = regexp.MustCompile(`\b(get|put|post|delete|patch)\s*:\s*"([^"]+)"`)

// addGatewayRoutes turns the grpc-gateway REST bindings on a line into
// http_route units handled by the annotated rpc method, so REST clients of a
// gateway service link to the same unit gRPC clients do.
func addGatewayRoutes(fc *fileCtx, line string, lineNo, rpcIdx int) {
	if rpcIdx < 0 || rpcIdx >= len(fc.units) {
		return
	}
	rpc := fc.units[rpcIdx]
	for _, m := range reGatewayBinding.FindAllStringSubmatch(line, -1) {
		method, path := strings.ToUpper(m[1]), m[2]
		u := &domain.ASTUnit{
			Kind:      store.KindHTTPRoute,
			Name:      method + " " + path,
			Qualified: routeKey(method, path),
			Signature: "path:" + path,
			StartLine: lineNo + 1,
			EndLine:   lineNo + 1,
			Hash:      hashString("gateway:" + method + " " + path),
		}
		fc.units = append(fc.units, u)
		fc.addEdge(len(fc.units)-1, store.EdgeHandledBy, rpc.Name, lineNo+1, contract.ConfHigh, nil)
	}
}

func prefixed(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// identAfter returns the identifier following a keyword prefix.
func identAfter(line, prefix string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	end := 0
	for end < len(rest) && (isIdentByte(rest[end]) || rest[end] == '.') {
		end++
	}
	return rest[:end]
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// parseRPC parses "rpc Name (Req) returns (Resp) {" lines.
func parseRPC(line string) (name, req, resp string) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "rpc "))
	i := strings.IndexAny(rest, " (")
	if i < 0 {
		return "", "", ""
	}
	name = rest[:i]

	extract := func(s string, from int) (string, int) {
		open := strings.Index(s[from:], "(")
		if open < 0 {
			return "", -1
		}
		open += from
		end := strings.Index(s[open:], ")")
		if end < 0 {
			return "", -1
		}
		val := strings.TrimSpace(s[open+1 : open+end])
		val = strings.TrimPrefix(val, "stream ")
		return strings.TrimSpace(val), open + end + 1
	}

	var next int
	req, next = extract(rest, i)
	if next < 0 {
		return name, "", ""
	}
	resp, _ = extract(rest, next)
	return name, req, resp
}

// parseField parses proto field lines: "[repeated|optional] Type name = N;".
func parseField(line string) (fieldType, fieldName string, ok bool) {
	if strings.HasPrefix(line, "option ") || strings.HasPrefix(line, "reserved ") ||
		strings.HasPrefix(line, "oneof ") {
		return "", "", false
	}
	eq := strings.Index(line, "=")
	if eq < 0 {
		return "", "", false
	}
	decl := strings.TrimSpace(line[:eq])
	fields := strings.Fields(decl)
	if len(fields) < 2 {
		return "", "", false
	}
	// strip labels
	if fields[0] == "repeated" || fields[0] == "optional" || fields[0] == "required" {
		fields = fields[1:]
	}
	if len(fields) < 2 {
		return "", "", false
	}
	fieldType = strings.Join(fields[:len(fields)-1], " ")
	fieldName = fields[len(fields)-1]
	if !isIdentLike(fieldName) {
		return "", "", false
	}
	return fieldType, fieldName, true
}
