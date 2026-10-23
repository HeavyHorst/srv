// Package identity resolves explicitly configured non-human service owners.
package identity

import (
	"fmt"
	"regexp"
	"strings"

	"tailscale.com/client/tailscale/apitype"

	"srv/internal/model"
)

// Providers maps stable Tailscale node IDs to service names, never user logins.
type Providers map[string]string

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func IsService(login string) bool { return strings.HasPrefix(strings.ToLower(login), "service:") }

func (p Providers) Validate(admins []string) error {
	for node, name := range p {
		if node == "" || node == "*" || strings.ContainsAny(node, " \t\r\n,=") || !serviceName.MatchString(name) {
			return fmt.Errorf("provider_nodes requires explicit stable node IDs mapped to lowercase service names (not user logins)")
		}
	}
	for _, admin := range admins {
		if IsService(strings.TrimSpace(admin)) {
			return fmt.Errorf("service owners cannot be admin users")
		}
	}
	return nil
}

// AllowsForwarded ensures every backend opts in to a forwarded service owner.
// As with human identities, the trusted frontend authenticates the original peer.
func (p Providers) AllowsForwarded(login string) bool {
	for _, name := range p {
		if login == "service:"+name {
			return true
		}
	}
	return false
}

func (p Providers) Resolve(who *apitype.WhoIsResponse, remote, sshUser string) (model.Actor, error) {
	if who == nil || who.Node == nil {
		return model.Actor{}, fmt.Errorf("tailscale whois response was incomplete")
	}
	a := model.Actor{NodeName: strings.TrimSuffix(who.Node.Name, "."), RemoteAddr: remote, SSHUser: sshUser}
	if who.Node.ComputedName != "" {
		a.NodeName = strings.TrimSuffix(who.Node.ComputedName, ".")
	}
	if name, ok := p[string(who.Node.StableID)]; ok && who.Node.StableID != "" {
		if !serviceName.MatchString(name) {
			return model.Actor{}, fmt.Errorf("invalid provider service name")
		}
		a.UserLogin, a.DisplayName = "service:"+name, "Service "+name
		return a, nil
	}
	if who.UserProfile == nil || who.UserProfile.LoginName == "" || len(who.Node.Tags) != 0 || IsService(who.UserProfile.LoginName) {
		return model.Actor{}, fmt.Errorf("command requires a Tailscale user identity or explicitly configured provider node")
	}
	a.UserLogin, a.DisplayName = who.UserProfile.LoginName, who.UserProfile.DisplayName
	return a, nil
}
