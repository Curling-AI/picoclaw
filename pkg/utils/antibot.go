package utils

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// AntiBotPage says which anti-bot product answered instead of the site.
type AntiBotPage struct {
	Vendor string
	Kind   string
}

const (
	AntiBotChallenge = "challenge"
	AntiBotBlock     = "block"

	VendorCloudflare = "cloudflare"
	VendorImperva    = "imperva"
	VendorPerimeterX = "perimeterx"
	VendorDataDome   = "datadome"
	VendorAkamai     = "akamai"
	VendorAWSWAF     = "aws-waf"
	VendorGoogle     = "google"
	VendorAmazon     = "amazon"
)

// Exact titles, lowercased. Only titles no real page would pick: this decides
// whether a page's content reaches the model, so a miss costs less than a false hit.
var antiBotTitles = map[string]AntiBotPage{
	"just a moment...":                    {VendorCloudflare, AntiBotChallenge},
	"just a moment…":                      {VendorCloudflare, AntiBotChallenge},
	"attention required! | cloudflare":    {VendorCloudflare, AntiBotBlock},
	"pardon our interruption":             {VendorImperva, AntiBotChallenge},
	"access to this page has been denied": {VendorPerimeterX, AntiBotBlock},
	"robot check":                         {VendorAmazon, AntiBotChallenge},
}

// Markers only trusted on a denied status: a page about these products may
// quote them, and Cloudflare's challenge-platform script runs on ordinary pages.
var antiBotBodyMarkers = []struct {
	marker string
	page   AntiBotPage
}{
	{"_cf_chl_opt", AntiBotPage{VendorCloudflare, AntiBotChallenge}},
	{"cf-error-details", AntiBotPage{VendorCloudflare, AntiBotBlock}},
	{"captcha-delivery.com", AntiBotPage{VendorDataDome, AntiBotChallenge}},
	{"px-captcha", AntiBotPage{VendorPerimeterX, AntiBotChallenge}},
	{"_Incapsula_Resource", AntiBotPage{VendorImperva, AntiBotChallenge}},
	{"errors.edgesuite.net", AntiBotPage{VendorAkamai, AntiBotBlock}},
	{"awswaf", AntiBotPage{VendorAWSWAF, AntiBotChallenge}},
}

var reHTMLTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// ClassifyAntiBotTitle recognizes an anti-bot page from what a browser reports
// after a navigation: its URL and title. "Access Denied" stays out: without the
// status and body it is as likely a site's own permission page.
func ClassifyAntiBotTitle(pageURL, title string) (AntiBotPage, bool) {
	if u, err := url.Parse(pageURL); err == nil && strings.HasPrefix(u.Path, "/sorry/") && isGoogleHost(u.Hostname()) {
		return AntiBotPage{VendorGoogle, AntiBotChallenge}, true
	}
	page, ok := antiBotTitles[strings.ToLower(strings.TrimSpace(html.UnescapeString(title)))]
	return page, ok
}

// ClassifyAntiBotResponse recognizes an anti-bot page from an HTTP response.
func ClassifyAntiBotResponse(status int, header http.Header, body string) (AntiBotPage, bool) {
	if strings.EqualFold(header.Get("Cf-Mitigated"), "challenge") {
		return AntiBotPage{VendorCloudflare, AntiBotChallenge}, true
	}
	switch strings.ToLower(header.Get("X-Amzn-Waf-Action")) {
	case "challenge", "captcha":
		return AntiBotPage{VendorAWSWAF, AntiBotChallenge}, true
	}
	if m := reHTMLTitle.FindStringSubmatch(body); m != nil {
		if page, ok := ClassifyAntiBotTitle("", m[1]); ok {
			return page, true
		}
	}
	if !isDeniedStatus(status) {
		return AntiBotPage{}, false
	}
	for _, m := range antiBotBodyMarkers {
		if strings.Contains(body, m.marker) {
			return m.page, true
		}
	}
	return AntiBotPage{}, false
}

func isGoogleHost(host string) bool {
	return strings.HasPrefix(host, "google.") || strings.Contains(host, ".google.")
}

func isDeniedStatus(status int) bool {
	switch status {
	case http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	}
	return false
}
