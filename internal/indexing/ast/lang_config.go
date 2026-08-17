package ast

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Nahua-Foundation/ragota/internal/contract"
	"github.com/Nahua-Foundation/ragota/internal/storage"

	"gopkg.in/yaml.v3"
)

// ConfigParser flattens configuration files (yaml, json, properties/env)
// into config_key units: qualified "config:<dot.path>", value in Signature.
// The linker uses these to resolve env/config-driven Kafka topics.
type ConfigParser struct {
	lang string
}

// NewConfigParser creates a config parser for "yaml", "json" or "properties".
func NewConfigParser(lang string) *ConfigParser { return &ConfigParser{lang: lang} }

// Language returns the language name.
func (p *ConfigParser) Language() string { return p.lang }

// maxConfigKeys caps the number of keys stored per file.
const maxConfigKeys = 500

// Parse extracts flattened config keys from the file.
func (p *ConfigParser) Parse(filePath, content string) ([]*storage.ASTUnit, []*storage.Edge, error) {
	values := map[string]string{}

	switch p.lang {
	case "yaml":
		var doc any
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			return nil, nil, nil // not a config document — skip silently
		}
		if routes := openAPIRoutes(doc); routes != nil {
			return routes, nil, nil
		}
		if channels := asyncAPIChannels(doc); channels != nil {
			return channels, nil, nil
		}
		flatten("", doc, values)
	case "json":
		var doc any
		if err := json.Unmarshal([]byte(content), &doc); err != nil {
			return nil, nil, nil
		}
		if routes := openAPIRoutes(doc); routes != nil {
			return routes, nil, nil
		}
		if channels := asyncAPIChannels(doc); channels != nil {
			return channels, nil, nil
		}
		flatten("", doc, values)
	case "properties":
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
				continue
			}
			sep := strings.IndexAny(line, "=:")
			if sep <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:sep])
			val := strings.TrimSpace(line[sep+1:])
			if key != "" && val != "" {
				values[key] = val
			}
		}
	default:
		return nil, nil, fmt.Errorf("unsupported config language: %s", p.lang)
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxConfigKeys {
		slog.Warn("config file exceeds key limit, truncating",
			"file", filePath, "limit", maxConfigKeys, "keys", len(keys))
		keys = keys[:maxConfigKeys]
	}

	var units []*storage.ASTUnit
	for _, key := range keys {
		// "${ORDERS_TOPIC:orders}" is the value the service runs with when the
		// environment does not set the key, so the default is the resolved
		// value. Plain "${KEY}" references are left for the linker.
		value := applyPlaceholderDefaults(values[key])
		units = append(units, &storage.ASTUnit{
			Kind:      storage.KindConfigKey,
			Name:      lastComponent(key),
			Qualified: contract.Config(key),
			Signature: value,
			StartLine: 1,
			EndLine:   1,
			Hash:      hashString(key + "=" + value),
		})
	}
	return units, nil, nil
}

var openAPIMethods = []string{"get", "post", "put", "delete", "patch", "head", "options"}

// openAPIRoutes turns an OpenAPI/Swagger document into http_route contract
// units. Returns nil if the document is not an OpenAPI spec.
func openAPIRoutes(doc any) []*storage.ASTUnit {
	root := asStringMap(doc)
	if root == nil {
		return nil
	}
	if root["openapi"] == nil && root["swagger"] == nil {
		return nil
	}
	paths := asStringMap(root["paths"])
	if paths == nil {
		return nil
	}
	base := openAPIBasePath(root)

	pathKeys := make([]string, 0, len(paths))
	for p := range paths {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)

	var units []*storage.ASTUnit
	for _, key := range pathKeys {
		ops := asStringMap(paths[key])
		if ops == nil {
			continue
		}
		// Clients request the server's base path plus the path item key; a
		// route stored without the base never matches them.
		path := joinPath(base, key)
		for _, method := range openAPIMethods {
			op := asStringMap(ops[method])
			if op == nil {
				continue
			}
			m := strings.ToUpper(method)
			doc := ""
			if s, ok := op["summary"].(string); ok {
				doc = s
			}
			units = append(units, &storage.ASTUnit{
				Kind:      storage.KindHTTPRoute,
				Name:      m + " " + path,
				Qualified: RouteKey(m, path),
				Signature: "path:" + path,
				Doc:       doc,
				StartLine: 1,
				EndLine:   1,
				Hash:      hashString("openapi:" + m + " " + path),
			})
		}
	}
	return units
}

// openAPIBasePath returns the path prefix every operation is served under:
// OpenAPI 3's first servers[].url path component, or Swagger 2's basePath.
// A server URL with a "{var}" template segment is skipped — the concrete
// prefix is unknown and guessing it would key routes on a path no client
// calls.
func openAPIBasePath(root map[string]any) string {
	if servers, ok := root["servers"].([]any); ok {
		for _, s := range servers {
			m := asStringMap(s)
			if m == nil {
				continue
			}
			raw, _ := m["url"].(string)
			if raw == "" {
				continue
			}
			_, path := splitURL(applyPlaceholderDefaults(raw))
			if strings.Contains(path, "{") {
				continue
			}
			if p := strings.Trim(path, "/"); p != "" {
				return p
			}
		}
		return ""
	}
	if bp, ok := root["basePath"].(string); ok {
		return strings.Trim(bp, "/")
	}
	return ""
}

// asyncAPIChannels turns an AsyncAPI document into topic_channel contract
// units. Returns nil if the document is not an AsyncAPI spec. Supports
// AsyncAPI 2.x (channel key is the topic name, operations under
// "publish"/"subscribe") and AsyncAPI 3.0 (topic name in "address",
// falling back to the channel key).
func asyncAPIChannels(doc any) []*storage.ASTUnit {
	root := asStringMap(doc)
	if root == nil {
		return nil
	}
	if v, ok := root["asyncapi"]; !ok || v == nil || v == "" {
		return nil
	}
	channels := asStringMap(root["channels"])

	type channelInfo struct {
		name        string
		description string
		ops         []string
	}
	infos := make([]channelInfo, 0, len(channels))
	for key, raw := range channels {
		ch := asStringMap(raw)
		if ch == nil {
			continue
		}
		ci := channelInfo{name: key}
		if addr, ok := ch["address"].(string); ok && addr != "" {
			ci.name = addr // AsyncAPI 3.0
		}
		if desc, ok := ch["description"].(string); ok {
			ci.description = desc
		}
		for _, op := range []string{"publish", "subscribe"} { // sorted
			if ch[op] != nil {
				ci.ops = append(ci.ops, op)
			}
		}
		infos = append(infos, ci)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].name < infos[j].name })

	units := make([]*storage.ASTUnit, 0, len(infos))
	for _, ci := range infos {
		sig := ""
		if len(ci.ops) > 0 {
			sig = "ops:" + strings.Join(ci.ops, ",")
		}
		units = append(units, &storage.ASTUnit{
			Kind:      storage.KindTopicChannel,
			Name:      ci.name,
			Qualified: contract.Topic(ci.name),
			Signature: sig,
			Doc:       ci.description,
			StartLine: 1,
			EndLine:   1,
			Hash:      hashString("asyncapi:" + ci.name),
		})
	}
	return units
}

// asStringMap coerces yaml/json decoded maps to map[string]any.
func asStringMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out
	}
	return nil
}

// flatten walks a decoded yaml/json document, emitting scalar leaves as
// dot-joined keys. Array elements use their index as a path segment.
func flatten(prefix string, v any, out map[string]string) {
	if len(out) > maxConfigKeys {
		return
	}
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			flatten(joinKey(prefix, k), child, out)
		}
	case map[any]any:
		for k, child := range val {
			flatten(joinKey(prefix, fmt.Sprint(k)), child, out)
		}
	case []any:
		for i, child := range val {
			flatten(joinKey(prefix, fmt.Sprint(i)), child, out)
		}
	case nil:
		// skip
	default:
		if prefix != "" {
			out[prefix] = fmt.Sprint(val)
		}
	}
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
