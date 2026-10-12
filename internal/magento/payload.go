// Package magento models the structured config payload that the operator
// derives from a MageStore + its resolved dependencies (database host,
// Redis endpoint, OpenSearch, RabbitMQ, ...).
//
// This intermediate representation is deliberately neutral between the
// two config-emission modes the operator supports (see ADR 002):
//
//	ECE-Tools mode:   emit three base64-encoded JSON blobs as env vars
//	                  (MAGENTO_CLOUD_RELATIONSHIPS / ROUTES / VARIABLES).
//	                  ECE-Tools inside the image generates env.php.
//	                  Suits shops coming from Adobe Commerce Cloud or
//	                  the PHOENIX-MEDIA helm-chart lineage.
//
//	env-vars mode:    flatten the same structures into individual env
//	                  vars (MAGENTO_DATABASE_HOST, MAGENTO_REDIS_HOST,
//	                  ...). The image's env.php uses getenv() to pick
//	                  them up. Suits traditional-hosting shops that do
//	                  not already ship ECE-Tools.
//
// Shapes borrow Magento Cloud naming (Relationships, Routes, Variables)
// because that is the existing vocabulary in the ecosystem, and because
// env-vars mode maps keys 1:1 from these structs.
//
// PHASE-1 CAVEAT: neither emission has been validated against a real
// Magento container. Version drift across 2.4.6/2.4.7/2.4.8 has not
// been exercised. testcontainers-go integration tests are required
// before marking config generation "production-ready".
package magento

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Relationships is the MAGENTO_CLOUD_RELATIONSHIPS top-level shape:
// a map from service name ("database", "redis", "opensearch", ...) to
// one-or-more endpoint records. Most services have exactly one record.
type Relationships map[string][]Endpoint

// Endpoint describes a single service connection. Field set taken from
// Platform.sh / Magento Cloud conventions. Not every field applies to
// every service — e.g. Path is the database name for MySQL, unused for
// Redis; Query carries driver-specific hints.
type Endpoint struct {
	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Scheme   string         `json:"scheme"`
	Username string         `json:"username,omitempty"`
	Password string         `json:"password,omitempty"`
	Path     string         `json:"path,omitempty"`
	Query    map[string]any `json:"query,omitempty"`
	Rel      string         `json:"rel,omitempty"`
	Service  string         `json:"service,omitempty"`
}

// Route is one entry in MAGENTO_CLOUD_ROUTES. The key (not stored here)
// is the incoming URL pattern, e.g. "https://{default}/".
type Route struct {
	Type     string `json:"type"` // "upstream" | "redirect"
	Upstream string `json:"upstream,omitempty"`
	To       string `json:"to,omitempty"`
}

// Routes maps URL patterns to routing decisions.
type Routes map[string]Route

// Variables is the flat MAGENTO_CLOUD_VARIABLES map. Keys follow the
// Magento Cloud convention of SCREAMING_SNAKE for top-level toggles
// and double-underscore path keys (e.g. CONFIG__DEFAULT__WEB__UNSECURE__BASE_URL)
// for config.xml overrides.
type Variables map[string]string

// DatabaseEndpoint is a convenience constructor for a MariaDB/MySQL
// endpoint. Dialect is "mysql"; this is the schema Magento expects.
func DatabaseEndpoint(host string, port int, database, user, password string) Endpoint {
	return Endpoint{
		Host:     host,
		Port:     port,
		Scheme:   "mysql",
		Path:     database,
		Username: user,
		Password: password,
		Rel:      "mysql",
		Service:  "database",
	}
}

// EncodeJSON marshals a value and base64-encodes the result. ECE-Tools
// expects its three env vars in this form.
func EncodeJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal ece-tools payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// BuildFrontendRoutes produces a minimal MAGENTO_CLOUD_ROUTES map routing
// everything on the given host to the app upstream. Multi-store hosts
// add themselves via AddStoreRoute once spec.stores is wired up.
func BuildFrontendRoutes(host string) Routes {
	return Routes{
		fmt.Sprintf("https://%s/", host): {Type: "upstream", Upstream: "app:http"},
		fmt.Sprintf("http://%s/", host):  {Type: "redirect", To: fmt.Sprintf("https://%s/", host)},
	}
}
