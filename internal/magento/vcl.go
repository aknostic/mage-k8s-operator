package magento

import (
	"fmt"
	"strings"
)

// DefaultVCLKey is the ConfigMap key the operator-generated default
// VCL is stored under. Matches the key the Varnish Deployment mounts
// at /etc/varnish/default.vcl. User-supplied VCL via
// `spec.varnish.vclConfigMapRef` MUST use the same key.
const DefaultVCLKey = "default.vcl"

// GenerateDefaultVCL returns a Varnish 4.1 VCL body suitable for
// fronting a Magento storefront. The generated body covers the v0.1.0
// demo cases:
//
//   - One backend named "default" pointing at <storeName>-frontend:8080
//     (in-namespace ClusterIP Service). Matches ensureVarnish's Service
//     port and ensureWorkloads' frontend container port.
//   - vcl_recv: bypass cache for /admin, /rest, /graphql, and any
//     non-GET/HEAD method (POSTs, authenticated mutations).
//   - vcl_backend_response: 1-hour fallback TTL when the backend
//     returns no Cache-Control header.
//
// When multiStore is true (caller passes len(spec.stores) > 0), the
// body adds an explicit vcl_hash subroutine that hashes Host + URL.
// Per ADR 007, multi-store shops share one Varnish in front of one
// frontend Deployment, and the per-store cache namespacing comes from
// the Host header. The Varnish 4.1 default vcl_hash already includes
// the host, but the explicit version makes the dependency on
// spec.stores visible in the generated body — operators auditing the
// ConfigMap can see the multi-store path is active.
//
// User-supplied VCL via `spec.varnish.vclConfigMapRef` overrides the
// generated body wholesale; the operator does not splice fragments
// into a user VCL. Custom VCL authors wanting per-store cache
// namespacing in multi-store setups must include hash_data(req.http.host)
// themselves; see docs/image-contract.md for a copyable snippet.
func GenerateDefaultVCL(storeName string, multiStore bool) string {
	var b strings.Builder
	b.WriteString("vcl 4.1;\n\n")
	fmt.Fprintf(&b, "backend default {\n    .host = %q;\n    .port = \"8080\";\n}\n\n", storeName+"-frontend")

	b.WriteString("sub vcl_recv {\n")
	b.WriteString("    # Magento admin and REST/GraphQL bypass the cache.\n")
	b.WriteString("    if (req.url ~ \"^/admin\" ||\n")
	b.WriteString("        req.url ~ \"^/rest\" ||\n")
	b.WriteString("        req.url ~ \"^/graphql\") {\n")
	b.WriteString("        return (pass);\n")
	b.WriteString("    }\n")
	b.WriteString("    # POSTs and authenticated mutations bypass.\n")
	b.WriteString("    if (req.method != \"GET\" && req.method != \"HEAD\") {\n")
	b.WriteString("        return (pass);\n")
	b.WriteString("    }\n")
	b.WriteString("    return (hash);\n")
	b.WriteString("}\n\n")

	if multiStore {
		b.WriteString("# Multi-store cache namespacing per ADR 007: each Host header\n")
		b.WriteString("# gets its own cache namespace because all hosts share one\n")
		b.WriteString("# frontend Deployment. Varnish 4.1 default vcl_hash already\n")
		b.WriteString("# includes the host, but the explicit version makes the\n")
		b.WriteString("# multi-store path visible in the generated body.\n")
		b.WriteString("sub vcl_hash {\n")
		b.WriteString("    hash_data(req.http.host);\n")
		b.WriteString("    hash_data(req.url);\n")
		b.WriteString("    return (lookup);\n")
		b.WriteString("}\n\n")
	}

	b.WriteString("sub vcl_backend_response {\n")
	b.WriteString("    # Do not cache 5xx responses; propagate to user instead.\n")
	b.WriteString("    if (beresp.status >= 500) {\n")
	b.WriteString("        set beresp.uncacheable = true;\n")
	b.WriteString("        set beresp.ttl = 0s;\n")
	b.WriteString("        return (deliver);\n")
	b.WriteString("    }\n")
	b.WriteString("    # Respect Magento's Cache-Control headers; 1-hour fallback.\n")
	b.WriteString("    if (beresp.ttl <= 0s) {\n")
	b.WriteString("        set beresp.ttl = 1h;\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")

	return b.String()
}
