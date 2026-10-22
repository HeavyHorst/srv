package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"srv/internal/model"
)

// Forwarding trusts the connecting node, never an SSH username or a caller's
// claimed node name. Keep the actual peer address in both audit records.
func (a *App) resolveCommandActor(ctx context.Context, sess gssh.Session, args []string) (model.Actor, []string, string, error) {
	remote := sess.RemoteAddr().String()
	who, err := a.localAPI.WhoIs(ctx, remote)
	if err != nil {
		return model.Actor{}, nil, "", err
	}
	if who == nil || who.Node == nil {
		return model.Actor{}, nil, "", errors.New("tailscale whois response was incomplete")
	}
	if len(args) > 0 && args[0] == "forward" {
		nodeID := string(who.Node.StableID)
		if nodeID == "" || !slices.Contains(a.cfg.TrustedFrontends, nodeID) {
			return model.Actor{}, nil, "", errors.New("identity forwarding requires a trusted frontend node")
		}
		if len(args) < 3 || len(args[1]) > 8192 {
			return model.Actor{}, nil, "", errors.New("invalid forwarded command")
		}
		data, err := base64.RawURLEncoding.DecodeString(args[1])
		var actor model.Actor
		if err != nil || json.Unmarshal(data, &actor) != nil || strings.TrimSpace(actor.UserLogin) == "" {
			return model.Actor{}, nil, "", errors.New("invalid forwarded actor")
		}
		if args[2] == "forward" || (args[2] == "--json" && (len(args) < 4 || args[3] == "forward")) {
			return model.Actor{}, nil, "", errors.New("nested identity forwarding is not allowed")
		}
		actor.RemoteAddr = remote
		actor.SSHUser = sess.User()
		return actor, args[2:], nodeID, nil
	}
	if who.UserProfile == nil || who.UserProfile.LoginName == "" || len(who.Node.Tags) != 0 {
		return model.Actor{}, nil, "", errors.New("command requires a Tailscale user identity or trusted frontend forwarding")
	}
	return model.Actor{
		UserLogin: who.UserProfile.LoginName, DisplayName: who.UserProfile.DisplayName,
		NodeName:   trimNodeName(who.Node.ComputedName, who.Node.Name),
		RemoteAddr: remote, SSHUser: sess.User(),
	}, args, "", nil
}
