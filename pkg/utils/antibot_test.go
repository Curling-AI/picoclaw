package utils

import (
	"net/http"
	"testing"
)

func TestClassifyAntiBotResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   AntiBotPage
		hit    bool
	}{
		{name: "cloudflare header", status: 403, header: http.Header{"Cf-Mitigated": {"challenge"}}, want: AntiBotPage{VendorCloudflare, AntiBotChallenge}, hit: true},
		{name: "aws waf header", status: 202, header: http.Header{"X-Amzn-Waf-Action": {"challenge"}}, want: AntiBotPage{VendorAWSWAF, AntiBotChallenge}, hit: true},
		{name: "challenge title on 200", status: 200, body: "<html><title>Just a moment...</title></html>", want: AntiBotPage{VendorCloudflare, AntiBotChallenge}, hit: true},
		{name: "escaped title", status: 403, body: "<title>Just a moment&#8230;</title>", want: AntiBotPage{VendorCloudflare, AntiBotChallenge}, hit: true},
		{name: "datadome body on 403", status: 403, body: `<script src="https://js.captcha-delivery.com/x.js"></script>`, want: AntiBotPage{VendorDataDome, AntiBotChallenge}, hit: true},
		{name: "cloudflare block body on 403", status: 403, body: `<div id="cf-error-details">`, want: AntiBotPage{VendorCloudflare, AntiBotBlock}, hit: true},
		{name: "article quoting a marker on 200", status: 200, body: "<title>How _cf_chl_opt works</title> _cf_chl_opt captcha-delivery.com"},
		{name: "article titled about the challenge", status: 200, body: "<title>Why you see Just a moment... on Cloudflare</title>"},
		{name: "ordinary page with the cloudflare script", status: 200, body: `<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>`},
		{name: "ordinary 403", status: 403, body: "<title>Forbidden</title>"},
		{name: "site's own access denied", status: 403, body: "<title>Access Denied</title>You must log in."},
		{name: "cloudflare managed but not challenged", status: 200, header: http.Header{"Cf-Mitigated": {"other"}, "Server": {"cloudflare"}}, body: "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header := c.header
			if header == nil {
				header = http.Header{}
			}
			got, hit := ClassifyAntiBotResponse(c.status, header, c.body)
			if hit != c.hit || got != c.want {
				t.Fatalf("got %+v, %v; want %+v, %v", got, hit, c.want, c.hit)
			}
		})
	}
}

func TestClassifyAntiBotTitle(t *testing.T) {
	cases := []struct {
		name, url, title string
		want             AntiBotPage
		hit              bool
	}{
		{name: "cloudflare", url: "https://www.jusbrasil.com.br/x", title: "  Just a moment...  ", want: AntiBotPage{VendorCloudflare, AntiBotChallenge}, hit: true},
		{name: "google sorry", url: "https://www.google.com/sorry/index?continue=x", title: "https://www.google.com/search", want: AntiBotPage{VendorGoogle, AntiBotChallenge}, hit: true},
		{name: "google regional", url: "https://www.google.com.br/sorry/index", want: AntiBotPage{VendorGoogle, AntiBotChallenge}, hit: true},
		{name: "lookalike host", url: "https://notgoogle.com/sorry/index", title: "Sorry"},
		{name: "another site's sorry page", url: "https://example.com/sorry/index", title: "Sorry"},
		{name: "ordinary page", url: "https://example.com", title: "Example Domain"},
		{name: "site's own access denied", url: "https://example.com", title: "Access Denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, hit := ClassifyAntiBotTitle(c.url, c.title)
			if hit != c.hit || got != c.want {
				t.Fatalf("got %+v, %v; want %+v, %v", got, hit, c.want, c.hit)
			}
		})
	}
}
