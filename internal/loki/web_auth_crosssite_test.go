package loki

import (
	"net/http/httptest"
	"testing"
)

func TestCrossSiteReject(t *testing.T) {
	t.Setenv("LOKI_TRUSTED_HOSTS", "loki.exemple.fr")
	cas := []struct {
		nom          string
		host, origin string
		fetchSite    string
		tunnel       bool
		keyed        bool
		refuse       bool
	}{
		{nom: "curl sur IP", host: "192.168.1.10:8090"},
		{nom: "localhost", host: "localhost:8090", origin: "http://localhost:8090"},
		{nom: "nom sans point", host: "tower:8090", origin: "http://tower:8090"},
		{nom: "nom .local", host: "tower.local:8090"},
		{nom: "origine tierce", host: "192.168.1.10:8090", origin: "http://evil.example", refuse: true},
		{nom: "origine null", host: "localhost:8090", origin: "null", refuse: true},
		{nom: "sec-fetch cross-site", host: "localhost:8090", fetchSite: "cross-site", refuse: true},
		{nom: "rebinding sans clé", host: "evil.example:8090", origin: "http://evil.example:8090", refuse: true},
		{nom: "domaine avec clé", host: "evil.example:8090", keyed: true},
		{nom: "domaine de confiance", host: "loki.exemple.fr", origin: "https://loki.exemple.fr"},
		{nom: "tunnel", host: "relais.exemple.net", tunnel: true},
	}
	for _, c := range cas {
		r := httptest.NewRequest("POST", "/api/status", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		if c.tunnel {
			r.Header.Set(viaTunnelHeader, "tunnel")
		}
		if got := crossSiteReject(r, c.keyed) != ""; got != c.refuse {
			t.Errorf("%s : refus=%v, attendu %v", c.nom, got, c.refuse)
		}
	}
}
