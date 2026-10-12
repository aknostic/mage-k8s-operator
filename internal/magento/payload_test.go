package magento

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDatabaseEndpointShape(t *testing.T) {
	ep := DatabaseEndpoint("mariadb", 3306, "magento", "magento", "s3cret")
	if ep.Scheme != "mysql" {
		t.Errorf("scheme: want mysql, got %q", ep.Scheme)
	}
	if ep.Path != "magento" {
		t.Errorf("database name (Path): want magento, got %q", ep.Path)
	}
	if ep.Rel != "mysql" || ep.Service != "database" {
		t.Errorf("rel/service: want mysql/database, got %q/%q", ep.Rel, ep.Service)
	}
}

func TestEncodeJSONRoundTrip(t *testing.T) {
	rel := Relationships{
		"database": {DatabaseEndpoint("mariadb-svc", 3306, "magento", "magento", "s3cret")},
	}

	encoded, err := EncodeJSON(rel)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}

	var got Relationships
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(got["database"]) != 1 || got["database"][0].Host != "mariadb-svc" {
		t.Fatalf("round-trip lost data: %#v", got)
	}
}

func TestBuildFrontendRoutes(t *testing.T) {
	routes := BuildFrontendRoutes("shop.example.com")
	https := routes["https://shop.example.com/"]
	if https.Type != "upstream" || https.Upstream != "app:http" {
		t.Errorf("https route: want upstream/app:http, got %+v", https)
	}
	httpRoute := routes["http://shop.example.com/"]
	if httpRoute.Type != "redirect" || httpRoute.To != "https://shop.example.com/" {
		t.Errorf("http route: want redirect to https, got %+v", httpRoute)
	}
}

// TestRelationshipsGoldenShape locks down the wire format. If this diffs,
// something about the shape that ECE-Tools expects has changed and we need
// to validate against a real Magento container before shipping.
func TestRelationshipsGoldenShape(t *testing.T) {
	rel := Relationships{
		"database": {DatabaseEndpoint("mariadb", 3306, "magento", "magento", "s3cret")},
	}
	raw, _ := json.Marshal(rel)
	got := string(raw)

	want := `{"database":[{"host":"mariadb","port":3306,"scheme":"mysql","username":"magento","password":"s3cret","path":"magento","rel":"mysql","service":"database"}]}`
	if got != want {
		t.Errorf("golden drift:\n got:  %s\n want: %s", got, want)
	}
}
