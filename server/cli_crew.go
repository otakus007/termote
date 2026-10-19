package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

var crewCallErrs = callErrs{
	noAuth:      "crew management over TCP requires sign-in, or use the local socket",
	unsupported: "crew federation is not available on this server",
	unknownCode: "unknown_member",
	unknownID:   "no crew member has this id (list them: termote crew status)",
	oldServer:   "the running server does not support crew federation; update it: termote update",
}

func (c *cli) crewStore() (*CrewStore, error) {
	if err := c.ensureStateDir(); err != nil {
		return nil, err
	}
	return NewCrewStore(filepath.Join(c.stateDir(), "crew"), "")
}

func (c *cli) defaultNodeURL(t deviceTarget) string {
	if p := strings.TrimRight(strings.TrimSpace(t.urls.primary()), "/"); p != "" {
		if after, ok := strings.CutPrefix(p, "http://"); ok {
			return "https://" + after
		}
		return p
	}
	return fmt.Sprintf("https://127.0.0.1:%d", t.port)
}

func (c *cli) cmdCrew(args []string) error {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status", "list":
		return c.cmdCrewStatus(args)
	case "invite":
		return c.cmdCrewInvite(args)
	case "join":
		return c.cmdCrewJoin(args)
	case "add":
		return c.cmdCrewAdd(args)
	case "remove", "rm":
		return c.cmdCrewRemove(args)
	case "deputy":
		return c.cmdCrewDeputy(args)
	case "promote":
		return c.cmdCrewPromote(args)
	default:
		return usageError("unknown crew command %q (expected: invite, join, add, status, remove, deputy, promote)", sub)
	}
}

func (c *cli) cmdCrewStatus(args []string) error {
	var asJSON bool
	fs := c.newFlagSet("crew status")
	fs.BoolVar(&asJSON, "json", false, "")
	if pos, err := parseArgs(fs, args); err != nil {
		return flagErr(err)
	} else if len(pos) > 0 {
		return usageError("crew status takes no positional arguments")
	}

	t := c.deviceTarget()
	var st CrewStatusResponse
	if err := c.serverCall(t, http.MethodGet, "/api/mux/crew/status", nil, &st, crewCallErrs); err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		mgr := NewCrewManager(store, nil, nil, nil, nil, nil, nil, hostAllowlist{})
		st = mgr.Status()
	}

	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}

	fmt.Fprintf(c.out, "Node:         %s (%s)\n", st.NodeName, st.NodeID)
	fmt.Fprintf(c.out, "Role:         %s\n", st.Role)
	fmt.Fprintf(c.out, "Fingerprint:  %s\n", st.CertSha256)
	if st.CrewID != "" {
		fmt.Fprintf(c.out, "Crew ID:      %s\n", st.CrewID)
	}
	if st.DeputyID != "" {
		fmt.Fprintf(c.out, "Deputy ID:    %s\n", st.DeputyID)
	}
	if st.Lead != nil {
		fmt.Fprintf(c.out, "Pinned Lead:  %s (%s) at %s\n", st.Lead.LeadName, st.Lead.LeadID, st.Lead.LeadURL)
	}
	if len(st.Members) == 0 {
		fmt.Fprintln(c.out, "\nNo federated crew members registered.")
		return nil
	}
	fmt.Fprintln(c.out, "")
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tSTATUS\tLATENCY\tURL")
	for _, m := range st.Members {
		lat := "-"
		if m.Status == "online" {
			lat = fmt.Sprintf("%dms", m.LatencyMs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Name, m.Role, m.Status, lat, m.URL)
	}
	return tw.Flush()
}

func (c *cli) cmdCrewInvite(args []string) error {
	var name, leadURL string
	fs := c.newFlagSet("crew invite")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&leadURL, "url", "", "")
	if pos, err := parseArgs(fs, args); err != nil {
		return flagErr(err)
	} else if len(pos) > 0 {
		return usageError("crew invite takes no positional arguments (use --name)")
	}

	t := c.deviceTarget()
	if leadURL == "" {
		leadURL = c.defaultNodeURL(t)
	}
	var res struct {
		Invite    string `json:"invite"`
		InviteID  string `json:"inviteId"`
		ExpiresAt string `json:"expiresAt"`
		LeadURL   string `json:"leadUrl"`
	}
	err := c.serverCall(t, http.MethodPost, "/api/mux/crew/invite", map[string]any{
		"name":    name,
		"leadUrl": leadURL,
	}, &res, crewCallErrs)
	if err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		tokStr, tok, cerr := store.CreateInvite(leadURL, name, defaultCrewInviteTTL, time.Now().UTC())
		if cerr != nil {
			return cerr
		}
		res.Invite = tokStr
		res.InviteID = tok.InviteID
		res.ExpiresAt = tok.ExpiresAt
		res.LeadURL = tok.LeadURL
	}

	fmt.Fprintf(c.out, "Single-use Crew invite (expires %s):\n\n  %s\n\nRun on the member machine:\n  termote crew join %s\n",
		res.ExpiresAt, res.Invite, res.Invite)
	return nil
}

func (c *cli) cmdCrewJoin(args []string) error {
	var name, memberURL string
	fs := c.newFlagSet("crew join")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&memberURL, "url", "", "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		return usageError("usage: termote crew join <invite-token> [--name <label>] [--url <member-url>]")
	}
	invite := strings.TrimSpace(pos[0])
	t := c.deviceTarget()
	if memberURL == "" {
		memberURL = c.defaultNodeURL(t)
	}

	var resp crewJoinResponse
	callErr := c.serverCall(t, http.MethodPost, "/api/mux/crew/join", map[string]any{
		"invite":    invite,
		"name":      name,
		"memberUrl": memberURL,
	}, &resp, crewCallErrs)
	if callErr != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return callErr
		}
		mgr := NewCrewManager(store, nil, nil, nil, nil, nil, nil, hostAllowlist{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, jerr := mgr.JoinUsingInvite(ctx, invite, name, memberURL)
		if jerr != nil {
			return jerr
		}
		resp = *r
	}

	fmt.Fprintf(c.out, "Joined crew %s led by %s (%s)\n", resp.CrewID, resp.LeadName, resp.LeadID)
	return nil
}

func (c *cli) cmdCrewAdd(args []string) error {
	var fingerprint, secretHex, name, id string
	fs := c.newFlagSet("crew add")
	fs.StringVar(&fingerprint, "fingerprint", "", "")
	fs.StringVar(&secretHex, "secret", "", "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&id, "id", "", "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		return usageError("usage: termote crew add <member-url> --fingerprint <sha256> --secret <hex> [--name <label>]")
	}
	memberURL := strings.TrimSpace(pos[0])
	if strings.TrimSpace(fingerprint) == "" || strings.TrimSpace(secretHex) == "" {
		return usageError("--fingerprint and --secret are required")
	}

	t := c.deviceTarget()
	var res map[string]any
	if err := c.serverCall(t, http.MethodPost, "/api/mux/crew/add", map[string]any{
		"id":         id,
		"name":       name,
		"url":        memberURL,
		"certSha256": fingerprint,
		"secretHex":  secretHex,
	}, &res, crewCallErrs); err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		if aerr := store.AddOrUpdateMember(CrewMember{
			ID:              id,
			Name:            name,
			URL:             memberURL,
			CertSha256:      fingerprint,
			SharedSecretHex: secretHex,
			Role:            CrewRoleMember,
		}); aerr != nil {
			return aerr
		}
	}

	fmt.Fprintf(c.out, "Added crew member %s\n", memberURL)
	return nil
}

func (c *cli) cmdCrewRemove(args []string) error {
	fs := c.newFlagSet("crew remove")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 1 {
		return usageError("usage: termote crew remove <member-id>")
	}
	memberID := strings.TrimSpace(pos[0])
	t := c.deviceTarget()
	var res map[string]any
	if err := c.serverCall(t, http.MethodPost, "/api/mux/crew/remove", map[string]any{
		"memberId": memberID,
	}, &res, crewCallErrs); err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		if _, ok, rerr := store.RemoveMember(memberID); rerr != nil {
			return rerr
		} else if !ok {
			return fmt.Errorf("member %q not found", memberID)
		}
	}
	fmt.Fprintf(c.out, "Removed crew member %s\n", memberID)
	return nil
}

func (c *cli) cmdCrewDeputy(args []string) error {
	var clearFlag bool
	fs := c.newFlagSet("crew deputy")
	fs.BoolVar(&clearFlag, "clear", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagErr(err)
	}
	target := ""
	if clearFlag {
		target = "--clear"
	} else if len(pos) == 1 {
		target = strings.TrimSpace(pos[0])
	} else {
		return usageError("usage: termote crew deputy <member-id|--clear>")
	}

	t := c.deviceTarget()
	var res struct {
		DeputyID string `json:"deputyId"`
	}
	if err := c.serverCall(t, http.MethodPost, "/api/mux/crew/deputy", map[string]any{
		"memberId": target,
	}, &res, crewCallErrs); err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		if _, derr := store.DesignateDeputy(target, time.Now().UTC()); derr != nil {
			return derr
		}
		res.DeputyID = store.StateSnapshot().DeputyID
	}

	if res.DeputyID == "" {
		fmt.Fprintln(c.out, "Cleared Standby Deputy designation.")
	} else {
		fmt.Fprintf(c.out, "Designated %s as Standby Deputy and signed Deputy Warrant.\n", res.DeputyID)
	}
	return nil
}

func (c *cli) cmdCrewPromote(args []string) error {
	fs := c.newFlagSet("crew promote")
	if pos, err := parseArgs(fs, args); err != nil {
		return flagErr(err)
	} else if len(pos) > 0 {
		return usageError("crew promote takes no arguments")
	}

	t := c.deviceTarget()
	var res map[string]any
	if err := c.serverCall(t, http.MethodPost, "/api/mux/crew/promote", map[string]any{}, &res, crewCallErrs); err != nil {
		store, serr := c.crewStore()
		if serr != nil {
			return err
		}
		if perr := store.PromoteDeputy(time.Now().UTC()); perr != nil {
			return perr
		}
	}
	fmt.Fprintln(c.out, "Promoted this node to active Crew Lead using Deputy Warrant.")
	return nil
}
