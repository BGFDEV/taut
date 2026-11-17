package graph

import (
	"strings"

	"github.com/bgfdev/taut/internal/fail"
)

// Cloud names accepted by LookupCloud.
const (
	CloudGlobal    = "global"
	CloudUSGov     = "usgov"     // GCC; uses the global endpoints
	CloudUSGovHigh = "usgovhigh" // GCC High
	CloudDoD       = "dod"
	CloudChina     = "china" // operated by 21Vianet
)

// Cloud is a national cloud deployment: where to get tokens and where
// Microsoft Graph lives.
type Cloud struct {
	// Name is the stable identifier used in config files.
	Name string
	// Authority is the Entra ID sign-in host, without a trailing slash.
	Authority string
	// GraphBase is the Microsoft Graph root, without a version segment.
	GraphBase string
}

// Clouds returns every supported cloud in a stable order.
func Clouds() []Cloud {
	return []Cloud{
		{CloudGlobal, "https://login.microsoftonline.com", "https://graph.microsoft.com"},
		{CloudUSGov, "https://login.microsoftonline.com", "https://graph.microsoft.com"},
		{CloudUSGovHigh, "https://login.microsoftonline.us", "https://graph.microsoft.us"},
		{CloudDoD, "https://login.microsoftonline.us", "https://dod-graph.microsoft.us"},
		{CloudChina, "https://login.partner.microsoftonline.cn", "https://microsoftgraph.chinacloudapi.cn"},
	}
}

// LookupCloud resolves a cloud by name, case-insensitively. The empty string
// means the global cloud. A few common aliases are accepted.
func LookupCloud(name string) (Cloud, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "", "public", "commercial":
		n = CloudGlobal
	case "gcc":
		n = CloudUSGov
	case "gcchigh", "gcc-high":
		n = CloudUSGovHigh
	case "21vianet":
		n = CloudChina
	}
	names := make([]string, 0, 5)
	for _, c := range Clouds() {
		if c.Name == n {
			return c, nil
		}
		names = append(names, c.Name)
	}
	return Cloud{}, fail.New(
		"entra.unknown_cloud",
		"The Microsoft cloud name is not recognized",
		"\""+name+"\" is not one of: "+strings.Join(names, ", ")+".",
		"Set the cloud in your config file to one of those names; use \"global\" for commercial and GCC tenants.",
	).WithExit(fail.ExitUsage)
}
