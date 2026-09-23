package ghinstance

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	ghauth "github.com/cli/go-gh/v2/pkg/auth"
)

// DefaultHostname is the domain name of the default GitHub instance.
const defaultHostname = "github.com"

// Localhost is the domain name of a local GitHub instance.
const localhost = "github.localhost"

// TenancyHost is the domain name of a tenancy GitHub instance.
const tenancyHost = "ghe.com"

// Default returns the host name of the default GitHub instance.
func Default() string {
	return defaultHostname
}

// TenantName extracts the tenant name from tenancy host name and
// reports whether it found the tenant name.
func TenantName(h string) (string, bool) {
	normalizedHostName := ghauth.NormalizeHostname(h)
	return strings.CutSuffix(normalizedHostName, "."+tenancyHost)
}

func isGarage(h string) bool {
	return strings.EqualFold(h, "garage.github.com")
}

func HostnameValidator(hostname string) error {
	if len(strings.TrimSpace(hostname)) < 1 {
		return errors.New("a value is required")
	}
	if strings.ContainsRune(hostname, '/') || strings.ContainsRune(hostname, ':') {
		return errors.New("invalid hostname")
	}
	return nil
}

func GraphQLEndpoint(hostname string) string {
	if baseURL := agsBaseURLForHost(hostname); baseURL != "" {
		return baseURL + "/api/graphql"
	}
	if isGarage(hostname) {
		return fmt.Sprintf("https://%s/api/graphql", hostname)
	}
	if ghauth.IsEnterprise(hostname) {
		return fmt.Sprintf("https://%s/api/graphql", hostname)
	}
	if strings.EqualFold(hostname, localhost) {
		return fmt.Sprintf("http://api.%s/graphql", hostname)
	}
	return fmt.Sprintf("https://api.%s/graphql", hostname)
}

func RESTPrefix(hostname string) string {
	if baseURL := agsBaseURLForHost(hostname); baseURL != "" {
		return baseURL + "/api/v3/"
	}
	if isGarage(hostname) {
		return fmt.Sprintf("https://%s/api/v3/", hostname)
	}
	if ghauth.IsEnterprise(hostname) {
		return fmt.Sprintf("https://%s/api/v3/", hostname)
	}
	if strings.EqualFold(hostname, localhost) {
		return fmt.Sprintf("http://api.%s/", hostname)
	}
	return fmt.Sprintf("https://api.%s/", hostname)
}

func GistPrefix(hostname string) string {
	prefix := "https://"
	if strings.EqualFold(hostname, localhost) {
		prefix = "http://"
	}
	return prefix + GistHost(hostname)
}

func GistHost(hostname string) string {
	if isGarage(hostname) {
		return fmt.Sprintf("%s/gist/", hostname)
	}
	if ghauth.IsEnterprise(hostname) {
		return fmt.Sprintf("%s/gist/", hostname)
	}
	if strings.EqualFold(hostname, localhost) {
		return fmt.Sprintf("%s/gist/", hostname)
	}
	return fmt.Sprintf("gist.%s/", hostname)
}

func HostPrefix(hostname string) string {
	if baseURL := agsBaseURLForHost(hostname); baseURL != "" {
		return baseURL + "/"
	}
	if strings.EqualFold(hostname, localhost) {
		return fmt.Sprintf("http://%s/", hostname)
	}
	return fmt.Sprintf("https://%s/", hostname)
}

func agsBaseURLForHost(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return ""
	}
	for _, envName := range []string{"AGS_URL", "AGENT_GIT_SERVICE_URL"} {
		raw := strings.TrimSpace(os.Getenv(envName))
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		if strings.EqualFold(hostname, u.Host) || strings.EqualFold(hostname, u.Hostname()) {
			return strings.TrimRight(raw, "/")
		}
	}
	return ""
}
