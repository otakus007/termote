package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultCrewPollInterval = 2 * time.Second
	defaultCrewPollTimeout  = 1500 * time.Millisecond
)

// CrewMemberStatus reports the live health and role of a Crew node in
// Snapshot.CrewMembers and /api/mux/crew/status.
type CrewMemberStatus struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	CertSha256 string `json:"certSha256,omitempty"`
	Role       string `json:"role"`
	Status     string `json:"status"` // "online" | "unreachable"
	LatencyMs  int64  `json:"latencyMs"`
	LastSeen   string `json:"lastSeen,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

// CrewLeadPublic is the non-sensitive Lead descriptor returned by status APIs.
type CrewLeadPublic struct {
	CrewID         string `json:"crewId"`
	LeadID         string `json:"leadId"`
	LeadName       string `json:"leadName"`
	LeadURL        string `json:"leadUrl"`
	LeadCertSha256 string `json:"leadCertSha256"`
	JoinedAt       string `json:"joinedAt,omitempty"`
	LastSeen       string `json:"lastSeen,omitempty"`
}

// CrewStatusResponse answers GET /api/mux/crew/status and `termote crew status`.
type CrewStatusResponse struct {
	CrewID       string             `json:"crewId,omitempty"`
	NodeID       string             `json:"nodeId"`
	NodeName     string             `json:"nodeName"`
	CertSha256   string             `json:"certSha256"`
	CertPEM      string             `json:"certPem,omitempty"`
	Role         string             `json:"role"`
	AdvertiseURL string             `json:"advertiseUrl,omitempty"`
	DeputyID     string             `json:"deputyId,omitempty"`
	Lead         *CrewLeadPublic    `json:"lead,omitempty"`
	Warrant      *DeputyWarrant     `json:"warrant,omitempty"`
	Members      []CrewMemberStatus `json:"members"`
	Roster       []CrewMemberPublic `json:"roster,omitempty"`
}

// MachineNodeStatus summarizes a local or federated Crew machine for MachinesView.
type MachineNodeStatus struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Role      string             `json:"role"`
	Status    string             `json:"status"`
	LatencyMs int64              `json:"latencyMs"`
	LastSeen  string             `json:"lastSeen,omitempty"`
	Host      MachineHostInfo    `json:"host"`
	Current   MachineSample      `json:"current"`
	Samples   []MachineSample    `json:"samples,omitempty"`
	Alerts    []MachineAlertRule `json:"alerts,omitempty"`
}

type crewMemberCache struct {
	status    CrewMemberStatus
	snapshot  *Snapshot
	machine   *MachineStatusResponse
	updatedAt time.Time
}

// CrewManager coordinates multi-machine Crew federation, background member
// polling, c:<memberId>/ ID namespacing, HTTP/WebSocket proxying, and Standby
// Deputy failover.
type CrewManager struct {
	store        *CrewStore
	mux          Mux
	innerMux     *http.ServeMux
	agent        *agentAPI
	machine      *machineCollector
	hub          *streamHub
	activity     *activityTracker
	allowed      hostAllowlist
	pollInterval time.Duration
	pollTimeout  time.Duration

	mu     sync.RWMutex
	cache  map[string]*crewMemberCache
	polled bool

	outTsMu   sync.Mutex
	lastOutTs int64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
}

func NewCrewManager(
	store *CrewStore,
	m Mux,
	innerMux *http.ServeMux,
	agent *agentAPI,
	machine *machineCollector,
	hub *streamHub,
	activity *activityTracker,
	allowed hostAllowlist,
) *CrewManager {
	cm := &CrewManager{
		store:        store,
		mux:          m,
		innerMux:     innerMux,
		agent:        agent,
		machine:      machine,
		hub:          hub,
		activity:     activity,
		allowed:      allowed,
		pollInterval: defaultCrewPollInterval,
		pollTimeout:  defaultCrewPollTimeout,
		cache:        make(map[string]*crewMemberCache),
		stopCh:       make(chan struct{}),
	}
	if innerMux != nil {
		cm.registerProtocolRoutes(innerMux)
		cm.registerAPIRoutes(innerMux)
	}
	return cm
}

func (m *CrewManager) Store() *CrewStore {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *CrewManager) Start(ctx context.Context) {
	if m == nil || m.store == nil {
		return
	}
	m.startOnce.Do(func() {
		go m.pollLoop(ctx)
	})
}

func (m *CrewManager) Close() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

func (m *CrewManager) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	m.PollOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.PollOnce()
		}
	}
}

func (m *CrewManager) nextTimestampString() string {
	m.outTsMu.Lock()
	defer m.outTsMu.Unlock()
	nowMs := time.Now().UTC().UnixMilli()
	if nowMs <= m.lastOutTs {
		nowMs = m.lastOutTs + 1
	}
	m.lastOutTs = nowMs
	return strconv.FormatInt(nowMs, 10)
}

func formatCrewID(memberID, origID string) string {
	if origID == "" {
		return ""
	}
	if strings.HasPrefix(origID, "c:") {
		return origID
	}
	return "c:" + memberID + "/" + origID
}

func parseCrewPrefixedID(rawID string) (memberID, origID string, ok bool) {
	s := strings.TrimSpace(rawID)
	if !strings.HasPrefix(s, "c:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "c:")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash >= len(rest)-1 {
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

func cloneCrewGroup(g Group) Group {
	ng := g
	if g.Worktree != nil {
		wt := *g.Worktree
		ng.Worktree = &wt
	}
	if g.Tabs != nil {
		ng.Tabs = make([]Tab, len(g.Tabs))
		for j, t := range g.Tabs {
			nt := t
			if t.Processes != nil {
				nt.Processes = slices.Clone(t.Processes)
			}
			if t.Panes != nil {
				nt.Panes = make([]Pane, len(t.Panes))
				for k, p := range t.Panes {
					np := p
					if p.Agent != nil {
						ag := *p.Agent
						np.Agent = &ag
					}
					if p.Process != nil {
						pr := *p.Process
						np.Process = &pr
					}
					nt.Panes[k] = np
				}
			}
			ng.Tabs[j] = nt
		}
	}
	return ng
}

func namespaceMemberSnapshot(mem CrewMember, snap Snapshot) Snapshot {
	out := snap
	out.Groups = make([]Group, len(snap.Groups))
	for i, g := range snap.Groups {
		ng := cloneCrewGroup(g)
		ng.ID = formatCrewID(mem.ID, g.ID)
		ng.MachineID = mem.ID
		ng.MachineName = mem.Name
		for j := range ng.Tabs {
			ng.Tabs[j].ID = formatCrewID(mem.ID, ng.Tabs[j].ID)
			ng.Tabs[j].Key = formatCrewID(mem.ID, ng.Tabs[j].Key)
			for k := range ng.Tabs[j].Panes {
				ng.Tabs[j].Panes[k].ID = formatCrewID(mem.ID, ng.Tabs[j].Panes[k].ID)
			}
		}
		out.Groups[i] = ng
	}
	return out
}

func namespaceMemberOverview(mem CrewMember, ov overviewResponse) overviewResponse {
	out := ov
	out.Panes = make([]overviewPane, len(ov.Panes))
	for i, p := range ov.Panes {
		np := p
		np.PaneID = formatCrewID(mem.ID, p.PaneID)
		np.Key = formatCrewID(mem.ID, p.Key)
		np.MachineID = mem.ID
		np.MachineName = mem.Name
		out.Panes[i] = np
	}
	return out
}

func (m *CrewManager) buildLocalSnapshot(r *http.Request) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(r.Context(), muxTimeout)
	defer cancel()
	var gen uint64
	if m.activity != nil {
		gen = m.activity.begin()
	}
	var (
		snap Snapshot
		err  error
	)
	if isViewOnly(r) {
		snap, err = viewSnapshot(ctx, m.mux)
	} else {
		snap, err = peekSnapshot(ctx, m.mux)
	}
	if err != nil {
		return Snapshot{}, err
	}
	if m.activity != nil {
		snap = m.activity.observe(gen, snap)
	}
	snap.APIVersion = apiVersion
	snap.Backend = m.mux.Name()
	snap.Caps = m.mux.Caps()
	if snap.Groups == nil {
		snap.Groups = []Group{}
	}
	if isViewOnly(r) {
		stripSnapshotDirs(&snap)
	}
	return snap, nil
}

func (m *CrewManager) buildLocalOverview(r *http.Request) (overviewResponse, error) {
	if m.agent == nil {
		return overviewResponse{
			ServerNow: time.Now().UTC().Format(activityTimeFormat),
			Panes:     []overviewPane{},
		}, nil
	}
	only := strings.TrimSpace(r.URL.Query().Get("pane"))
	var read overviewRead
	switch {
	case only == "":
		read, _ = m.agent.overview.do("", func() (overviewRead, error) {
			return m.agent.readOverview(""), nil
		})
	case m.agent.agentPaneInSnapshot(only):
		read, _ = m.agent.overview.do("pane\x00"+only, func() (overviewRead, error) {
			return m.agent.readOverview(only), nil
		})
	default:
		read = overviewRead{panes: []overviewPane{}}
	}
	res := overviewResponse{
		ServerNow: time.Now().UTC().Format(activityTimeFormat),
		Panes:     make([]overviewPane, len(read.panes)),
		Accounts:  read.accounts,
	}
	viewOnly := isViewOnly(r)
	for i, p := range read.panes {
		if c := p.change; c != nil && c.status && !(viewOnly && c.viewHidden) {
			n := c.changed
			p.Branch, p.Changed, p.ChangedTruncated = c.branch, &n, c.truncated
		}
		res.Panes[i] = p
	}
	return res, nil
}

func (m *CrewManager) buildLocalMachineStatus(r *http.Request) MachineStatusResponse {
	rangeParam := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeParam == "" {
		rangeParam = "1h"
	}
	resp := m.machine.Status(rangeParam, viewHidesMachineMounts(r))
	if m.store != nil {
		ident := m.store.Identity()
		resp.Host.ID = ident.ID
		if ident.Name != "" {
			resp.Host.Name = ident.Name
		}
	}
	return resp
}

func (m *CrewManager) doMemberRequest(
	ctx context.Context,
	mem CrewMember,
	method string,
	reqPath string,
	body []byte,
	clientRole role,
) (*http.Response, []byte, error) {
	return m.doMemberRequestWithContentType(ctx, mem, method, reqPath, body, "", clientRole)
}

func (m *CrewManager) doMemberRequestWithContentType(
	ctx context.Context,
	mem CrewMember,
	method string,
	reqPath string,
	body []byte,
	contentType string,
	clientRole role,
) (*http.Response, []byte, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(mem.URL), "/")
	if baseURL == "" {
		return nil, nil, errors.New("member URL is empty")
	}
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	fullURL := baseURL + reqPath
	parsedURL, err := url.Parse(fullURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid member URL %q: %w", fullURL, err)
	}
	if strings.EqualFold(parsedURL.Scheme, "http") {
		parsedURL.Scheme = "https"
		fullURL = parsedURL.String()
	}

	ident := m.store.Identity()
	ts := m.nextTimestampString()
	sigPath := parsedURL.RequestURI()
	sig, err := signCrewRequest(ident.PrivateKey, method, sigPath, body, ts)
	if err != nil {
		return nil, nil, err
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set(headerCrewTimestamp, ts)
	req.Header.Set(headerCrewSignature, sig)
	req.Header.Set(headerCrewSenderID, ident.ID)
	if clientRole == "" {
		clientRole = roleView
	}
	req.Header.Set(headerCrewClientRole, string(clientRole))
	if mem.SharedSecretHex != "" {
		if macHex, merr := computeCrewAuthHMAC(mem.SharedSecretHex, method, sigPath, body, ts); merr == nil {
			req.Header.Set(headerCrewAuthHMAC, macHex)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	tr := &http.Transport{
		TLSClientConfig:   m.store.ClientTLSConfigForPeer(mem.CertSha256),
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxCrewProxyBody+1))
	if err != nil {
		return resp, nil, err
	}
	if len(respBytes) > maxCrewProxyBody {
		return resp, nil, errors.New("member response exceeded 16 MiB limit")
	}
	return resp, respBytes, nil
}

// PollOnce polls all registered Crew members concurrently with a 1.5s timeout
// and updates the in-memory snapshot and machine status cache.
func (m *CrewManager) PollOnce() {
	if m == nil || m.store == nil {
		return
	}
	members := m.store.Members()
	if len(members) == 0 {
		return
	}
	st := m.store.StateSnapshot()
	var wg sync.WaitGroup
	for _, mem := range members {
		wg.Add(1)
		go func(mem CrewMember) {
			defer wg.Done()
			m.pollMember(mem, st.DeputyID)
		}(mem)
	}
	wg.Wait()

	m.mu.Lock()
	m.polled = true
	m.mu.Unlock()
}

func (m *CrewManager) pollMember(mem CrewMember, deputyID string) {
	ctx, cancel := context.WithTimeout(context.Background(), m.pollTimeout)
	defer cancel()

	t0 := time.Now()
	resp, body, err := m.doMemberRequest(ctx, mem, http.MethodGet, "/crew/v1/snapshot", nil, roleFull)
	latency := time.Since(t0).Milliseconds()

	roleName := mem.Role
	if deputyID != "" && mem.ID == deputyID {
		roleName = CrewRoleDeputy
	} else if roleName == "" {
		roleName = CrewRoleMember
	}

	m.mu.Lock()
	entry, ok := m.cache[mem.ID]
	if !ok {
		entry = &crewMemberCache{}
		m.cache[mem.ID] = entry
	}
	entry.status.ID = mem.ID
	entry.status.Name = mem.Name
	entry.status.URL = mem.URL
	entry.status.CertSha256 = mem.CertSha256
	entry.status.Role = roleName

	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		errMsg := "request failed"
		if err != nil {
			errMsg = err.Error()
		} else if resp != nil {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		entry.status.Status = "unreachable"
		entry.status.LastError = errMsg
		if entry.status.LastSeen == "" {
			entry.status.LastSeen = mem.LastSeen
		}
		m.mu.Unlock()
		return
	}

	var snap Snapshot
	if jerr := json.Unmarshal(body, &snap); jerr != nil {
		entry.status.Status = "unreachable"
		entry.status.LastError = "invalid snapshot JSON"
		m.mu.Unlock()
		return
	}

	namespaced := namespaceMemberSnapshot(mem, snap)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	entry.status.Status = "online"
	entry.status.LatencyMs = latency
	entry.status.LastSeen = nowStr
	entry.status.LastError = ""
	entry.snapshot = &namespaced
	entry.updatedAt = time.Now().UTC()
	m.mu.Unlock()

	// Also fetch machine status in the same poll cycle.
	mctx, mcancel := context.WithTimeout(context.Background(), m.pollTimeout)
	defer mcancel()
	if mresp, mbody, merr := m.doMemberRequest(mctx, mem, http.MethodGet, "/crew/v1/machine?range=1h", nil, roleFull); merr == nil && mresp != nil && mresp.StatusCode == http.StatusOK {
		var ms MachineStatusResponse
		if json.Unmarshal(mbody, &ms) == nil {
			ms.Host.ID = mem.ID
			if mem.Name != "" {
				ms.Host.Name = mem.Name
			}
			m.mu.Lock()
			if c, ok := m.cache[mem.ID]; ok {
				c.machine = &ms
			}
			m.mu.Unlock()
		}
	}
}

func (m *CrewManager) removeMemberCache(memberID string) {
	m.mu.Lock()
	delete(m.cache, memberID)
	m.mu.Unlock()
}

func (m *CrewManager) markMemberUnreachable(memberID string, err error) {
	m.mu.Lock()
	if entry, ok := m.cache[memberID]; ok {
		entry.status.Status = "unreachable"
		if err != nil {
			entry.status.LastError = err.Error()
		}
	}
	m.mu.Unlock()
}

// AugmentSnapshot merges cached federated Member groups and CrewMembers health
// status into the Lead's local Snapshot without performing blocking network I/O.
func (m *CrewManager) AugmentSnapshot(local Snapshot) Snapshot {
	if m == nil || m.store == nil {
		return local
	}
	members := m.store.Members()
	if len(members) == 0 {
		return local
	}

	m.mu.RLock()
	hasPolled := m.polled
	m.mu.RUnlock()
	if !hasPolled {
		m.PollOnce()
	}

	ident := m.store.Identity()
	st := m.store.StateSnapshot()
	for i := range local.Groups {
		if local.Groups[i].MachineID == "" {
			local.Groups[i].MachineID = ident.ID
		}
		if local.Groups[i].MachineName == "" {
			local.Groups[i].MachineName = ident.Name
		}
	}

	statuses := make([]CrewMemberStatus, 0, len(members)+1)
	statuses = append(statuses, CrewMemberStatus{
		ID:         ident.ID,
		Name:       ident.Name,
		URL:        st.AdvertiseURL,
		CertSha256: ident.CertSha256,
		Role:       CrewRoleLead,
		Status:     "online",
		LatencyMs:  0,
		LastSeen:   time.Now().UTC().Format(time.RFC3339),
	})

	m.mu.RLock()
	for _, mem := range members {
		roleName := mem.Role
		if st.DeputyID != "" && mem.ID == st.DeputyID {
			roleName = CrewRoleDeputy
		} else if roleName == "" {
			roleName = CrewRoleMember
		}
		entry, ok := m.cache[mem.ID]
		if !ok {
			statuses = append(statuses, CrewMemberStatus{
				ID:         mem.ID,
				Name:       mem.Name,
				URL:        mem.URL,
				CertSha256: mem.CertSha256,
				Role:       roleName,
				Status:     "unreachable",
				LastSeen:   mem.LastSeen,
			})
			continue
		}
		stItem := entry.status
		stItem.Role = roleName
		statuses = append(statuses, stItem)
		if stItem.Status == "online" && entry.snapshot != nil {
			for _, g := range entry.snapshot.Groups {
				local.Groups = append(local.Groups, cloneCrewGroup(g))
			}
		}
	}
	m.mu.RUnlock()

	local.CrewMembers = statuses
	return local
}

// AugmentOverview merges federated Crew member overviews into the Lead's overview.
func (m *CrewManager) AugmentOverview(r *http.Request, local overviewResponse) overviewResponse {
	if m == nil || m.store == nil {
		return local
	}
	members := m.store.Members()
	if len(members) == 0 {
		return local
	}
	ident := m.store.Identity()
	for i := range local.Panes {
		if local.Panes[i].MachineID == "" {
			local.Panes[i].MachineID = ident.ID
		}
		if local.Panes[i].MachineName == "" {
			local.Panes[i].MachineName = ident.Name
		}
	}

	callerRole := requestRole(r.Context())
	only := strings.TrimSpace(r.URL.Query().Get("pane"))
	if only != "" {
		memberID, origPane, hasPrefix := parseCrewPrefixedID(only)
		if !hasPrefix {
			return local
		}
		mem, ok := m.store.GetMember(memberID)
		if !ok {
			return local
		}
		m.mu.RLock()
		c := m.cache[mem.ID]
		offline := c != nil && c.status.Status == "unreachable"
		m.mu.RUnlock()
		if offline {
			return local
		}
		ctx, cancel := context.WithTimeout(r.Context(), m.pollTimeout)
		defer cancel()
		reqPath := "/crew/v1/overview?pane=" + url.QueryEscape(origPane)
		resp, body, err := m.doMemberRequest(ctx, mem, http.MethodGet, reqPath, nil, callerRole)
		if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
			return local
		}
		var ov overviewResponse
		if json.Unmarshal(body, &ov) != nil {
			return local
		}
		namespaced := namespaceMemberOverview(mem, ov)
		local.Panes = append(local.Panes, namespaced.Panes...)
		local.Accounts = append(local.Accounts, namespaced.Accounts...)
		return local
	}

	type memOv struct {
		idx int
		ov  overviewResponse
		ok  bool
	}
	results := make([]memOv, len(members))
	var wg sync.WaitGroup
	for i, mem := range members {
		m.mu.RLock()
		c := m.cache[mem.ID]
		offline := c != nil && c.status.Status == "unreachable"
		m.mu.RUnlock()
		if offline {
			continue
		}
		wg.Add(1)
		go func(i int, mem CrewMember) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), m.pollTimeout)
			defer cancel()
			resp, body, err := m.doMemberRequest(ctx, mem, http.MethodGet, "/crew/v1/overview", nil, callerRole)
			if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
				return
			}
			var ov overviewResponse
			if json.Unmarshal(body, &ov) != nil {
				return
			}
			results[i] = memOv{idx: i, ov: namespaceMemberOverview(mem, ov), ok: true}
		}(i, mem)
	}
	wg.Wait()

	for _, res := range results {
		if !res.ok {
			continue
		}
		local.Panes = append(local.Panes, res.ov.Panes...)
		local.Accounts = append(local.Accounts, res.ov.Accounts...)
	}
	return local
}

// AugmentMachineStatus adds multi-host Crew machine telemetry and handles
// `?machine=<memberId>` host switching for MachinesView.
func (m *CrewManager) AugmentMachineStatus(r *http.Request, local MachineStatusResponse) MachineStatusResponse {
	if m == nil || m.store == nil {
		return local
	}
	members := m.store.Members()
	if len(members) == 0 {
		return local
	}

	ident := m.store.Identity()
	st := m.store.StateSnapshot()
	local.Host.ID = ident.ID
	if ident.Name != "" {
		local.Host.Name = ident.Name
	}

	viewOnly := viewHidesMachineMounts(r)
	rangeParam := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeParam == "" {
		rangeParam = "1h"
	}
	selectedMachine := strings.TrimSpace(r.URL.Query().Get("machine"))

	nodes := make([]MachineNodeStatus, 0, len(members)+1)
	nodes = append(nodes, MachineNodeStatus{
		ID:        ident.ID,
		Name:      ident.Name,
		Role:      CrewRoleLead,
		Status:    "online",
		LatencyMs: 0,
		LastSeen:  time.Now().UTC().Format(time.RFC3339),
		Host:      local.Host,
		Current:   local.Current,
		Samples:   local.Samples,
		Alerts:    local.Alerts,
	})

	callerRole := requestRole(r.Context())
	for _, mem := range members {
		roleName := mem.Role
		if st.DeputyID != "" && mem.ID == st.DeputyID {
			roleName = CrewRoleDeputy
		} else if roleName == "" {
			roleName = CrewRoleMember
		}

		var (
			ms        *MachineStatusResponse
			stSummary CrewMemberStatus
		)
		m.mu.RLock()
		if entry, ok := m.cache[mem.ID]; ok {
			stSummary = entry.status
			if entry.machine != nil {
				cp := *entry.machine
				ms = &cp
			}
		}
		m.mu.RUnlock()

		// If this specific machine was requested or cache is empty, fetch live.
		if (selectedMachine == mem.ID || ms == nil) && stSummary.Status != "unreachable" {
			ctx, cancel := context.WithTimeout(r.Context(), m.pollTimeout)
			resp, body, err := m.doMemberRequest(ctx, mem, http.MethodGet, "/crew/v1/machine?range="+url.QueryEscape(rangeParam), nil, callerRole)
			cancel()
			if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
				var fetched MachineStatusResponse
				if json.Unmarshal(body, &fetched) == nil {
					fetched.Host.ID = mem.ID
					if mem.Name != "" {
						fetched.Host.Name = mem.Name
					}
					ms = &fetched
					stSummary.Status = "online"
					stSummary.LastSeen = time.Now().UTC().Format(time.RFC3339)
				}
			} else if stSummary.Status == "" {
				stSummary.Status = "unreachable"
			}
		}

		node := MachineNodeStatus{
			ID:        mem.ID,
			Name:      mem.Name,
			Role:      roleName,
			Status:    stSummary.Status,
			LatencyMs: stSummary.LatencyMs,
			LastSeen:  stSummary.LastSeen,
			Host:      MachineHostInfo{ID: mem.ID, Name: mem.Name},
			Current:   MachineSample{Disks: []MachineDiskStat{}},
		}
		if node.Status == "" {
			node.Status = "unreachable"
		}
		if ms != nil {
			redacted := *ms
			if viewOnly {
				redacted.Current.Disks = redactMachineDisks(redacted.Current.Disks)
				samplesCopy := make([]MachineSample, len(redacted.Samples))
				for i, s := range redacted.Samples {
					sc := cloneMachineSample(s)
					sc.Disks = redactMachineDisks(sc.Disks)
					samplesCopy[i] = sc
				}
				redacted.Samples = samplesCopy
			}
			node.Host = redacted.Host
			node.Current = redacted.Current
			node.Samples = redacted.Samples
			node.Alerts = redacted.Alerts
			if selectedMachine == mem.ID {
				local.Host = redacted.Host
				local.Current = redacted.Current
				local.Samples = redacted.Samples
				local.Alerts = redacted.Alerts
			}
		}
		nodes = append(nodes, node)
	}

	local.Machines = nodes
	return local
}

// WrapMux intercepts /api/mux/* requests targeting `c:<memberId>/<origId>`
// resources and proxies them to the target Crew member over TLS 1.3 mTLS
// while propagating the caller's role (`full` vs `view`).
func (m *CrewManager) WrapMux(inner *http.ServeMux) http.Handler {
	if m == nil {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if memberID, rewrittenPath, rewrittenEscaped, ok := extractCrewTargetPath(r.URL.Path); ok {
			m.proxyHTTPToMember(w, r, memberID, rewrittenPath, rewrittenEscaped, nil)
			return
		}
		// Intercept POST /api/mux/tabs when body.groupId has `c:<memberId>/` prefix.
		if r.URL.Path == "/api/mux/tabs" && r.Method == http.MethodPost {
			if !requireWriteRole(w, r) {
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
			if err != nil {
				jsonError(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			if len(body) > maxJSONBody {
				jsonError(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			var payload map[string]any
			if json.Unmarshal(body, &payload) == nil {
				if gid, ok := payload["groupId"].(string); ok {
					if memberID, origGroupID, hasPrefix := parseCrewPrefixedID(gid); hasPrefix {
						payload["groupId"] = origGroupID
						rewrittenBody, _ := json.Marshal(payload)
						m.proxyHTTPToMember(w, r, memberID, "/api/mux/tabs", "/api/mux/tabs", rewrittenBody)
						return
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		inner.ServeHTTP(w, r)
	})
}

func extractCrewTargetPath(reqPath string) (memberID, targetPath, targetEscapedPath string, ok bool) {
	prefixes := []string{
		"/api/mux/panes/",
		"/api/mux/tabs/",
		"/api/mux/groups/",
		"/api/mux/worktrees/",
	}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(reqPath, prefix) {
			continue
		}
		after := strings.TrimPrefix(reqPath, prefix)
		if !strings.HasPrefix(after, "c:") {
			continue
		}
		rest := strings.TrimPrefix(after, "c:")
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 || slash >= len(rest)-1 {
			continue
		}
		memberID = rest[:slash]
		tail := rest[slash+1:]
		origID := tail
		suffix := ""
		if nextSlash := strings.IndexByte(tail, '/'); nextSlash >= 0 {
			origID = tail[:nextSlash]
			suffix = tail[nextSlash:]
		}
		if origID == "" {
			continue
		}
		targetPath = prefix + origID + suffix
		targetEscapedPath = prefix + url.PathEscape(origID) + suffix
		return memberID, targetPath, targetEscapedPath, true
	}
	return "", "", "", false
}

func (m *CrewManager) proxyHTTPToMember(
	w http.ResponseWriter,
	r *http.Request,
	memberID string,
	targetPath string,
	targetEscapedPath string,
	overrideBody []byte,
) {
	// R5.5: Enforce view-only write rejection and cross-site read rejection on
	// the Lead before proxying, AND forward X-Termote-Client-Role so the Member
	// enforces view-only rules too.
	if isWriteMethod(r.Method) {
		if !requireWriteRole(w, r) {
			return
		}
	} else if msg := crossSiteRejection(m.allowed, r); msg != "" {
		jsonError(w, msg, http.StatusForbidden)
		return
	}
	callerRole := requestRole(r.Context())

	mem, ok := m.store.GetMember(memberID)
	if !ok {
		jsonError(w, fmt.Sprintf("unknown crew member %q", memberID), http.StatusBadGateway)
		return
	}

	var body []byte
	if overrideBody != nil {
		body = overrideBody
	} else if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxCrewProxyBody+1))
		if err != nil {
			jsonError(w, "read request body failed", http.StatusBadRequest)
			return
		}
		if len(body) > maxCrewProxyBody {
			jsonError(w, "request body exceeds 16 MiB limit", http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Rewrite query parameters `key` or `pane` if they carry `c:<memberId>/` prefix.
	q := r.URL.Query()
	queryChanged := false
	for _, qk := range []string{"key", "pane", "paneId"} {
		if qv := q.Get(qk); qv != "" {
			if _, origVal, hasPref := parseCrewPrefixedID(qv); hasPref {
				q.Set(qk, origVal)
				queryChanged = true
			}
		}
	}
	rawQuery := r.URL.RawQuery
	if queryChanged {
		rawQuery = q.Encode()
	}

	proxyReqPath := "/crew/v1/proxy" + targetEscapedPath
	if rawQuery != "" {
		proxyReqPath += "?" + rawQuery
	}

	contentType := r.Header.Get("Content-Type")
	if overrideBody != nil && contentType == "" {
		contentType = "application/json"
	}

	ctx, cancel := context.WithTimeout(r.Context(), muxTimeout)
	defer cancel()
	resp, respBody, err := m.doMemberRequestWithContentType(ctx, mem, r.Method, proxyReqPath, body, contentType, callerRole)
	if err != nil {
		m.markMemberUnreachable(memberID, err)
		jsonError(w, fmt.Sprintf("crew member %s unreachable: %v", mem.Name, err), http.StatusBadGateway)
		return
	}

	for k, vals := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding":
			continue
		default:
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// ProxyStream proxies a WebSocket terminal stream (`GET /api/mux/stream`) for
// a `c:<memberId>/<origPane>` pane over TLS 1.3 mTLS to the target Member.
func (m *CrewManager) ProxyStream(
	w http.ResponseWriter,
	r *http.Request,
	memberID string,
	origPane string,
	view bool,
	deviceID string,
	sessionID string,
) {
	mem, ok := m.store.GetMember(memberID)
	if !ok {
		jsonError(w, fmt.Sprintf("unknown crew member %q", memberID), http.StatusBadRequest)
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(mem.URL), "/")
	parsedBase, err := url.Parse(baseURL)
	if err != nil || parsedBase.Host == "" {
		jsonError(w, "invalid member URL", http.StatusBadGateway)
		return
	}

	wsScheme := "wss"
	q := url.Values{}
	q.Set("pane", origPane)
	if cols := r.URL.Query().Get("cols"); cols != "" {
		q.Set("cols", cols)
	}
	if rows := r.URL.Query().Get("rows"); rows != "" {
		q.Set("rows", rows)
	}
	reqPath := "/crew/v1/stream?" + q.Encode()
	wsURL := (&url.URL{
		Scheme:   wsScheme,
		Host:     parsedBase.Host,
		Path:     "/crew/v1/stream",
		RawQuery: q.Encode(),
	}).String()

	ident := m.store.Identity()
	ts := m.nextTimestampString()
	sig, err := signCrewRequest(ident.PrivateKey, http.MethodGet, reqPath, nil, ts)
	if err != nil {
		jsonError(w, "failed to sign stream request", http.StatusInternalServerError)
		return
	}
	hdr := http.Header{}
	hdr.Set(headerCrewTimestamp, ts)
	hdr.Set(headerCrewSignature, sig)
	hdr.Set(headerCrewSenderID, ident.ID)
	streamRole := roleFull
	if view {
		streamRole = roleView
	}
	hdr.Set(headerCrewClientRole, string(streamRole))
	if mem.SharedSecretHex != "" {
		if macHex, merr := computeCrewAuthHMAC(mem.SharedSecretHex, http.MethodGet, reqPath, nil, ts); merr == nil {
			hdr.Set(headerCrewAuthHMAC, macHex)
		}
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: m.store.ClientTLSConfigForPeer(mem.CertSha256),
		},
	}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), muxTimeout)
	peerConn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: hdr,
	})
	dialCancel()
	if err != nil {
		m.markMemberUnreachable(memberID, err)
		jsonError(w, fmt.Sprintf("member stream dial failed: %v", err), http.StatusBadGateway)
		return
	}
	peerConn.SetReadLimit(maxStreamMessage)

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		peerConn.CloseNow()
		return
	}
	clientConn.SetReadLimit(maxStreamMessage)

	sctx, scancel := context.WithCancelCause(context.Background())
	var entry *hubEntry
	if m.hub != nil {
		entry, err = m.hub.addFor(scancel, streamRole, deviceID, sessionID)
		if err != nil {
			peerConn.CloseNow()
			clientConn.Close(closeEvicted, err.Error())
			return
		}
		m.hub.setPane(entry, formatCrewID(memberID, origPane))
		defer m.hub.remove(entry)
	}
	defer scancel(nil)
	defer peerConn.CloseNow()
	defer clientConn.CloseNow()

	errCh := make(chan error, 2)
	// Member -> Client
	go func() {
		for {
			typ, data, rerr := peerConn.Read(sctx)
			if rerr != nil {
				errCh <- rerr
				return
			}
			if werr := writeFrame(clientConn, typ, data); werr != nil {
				errCh <- werr
				return
			}
		}
	}()
	// Client -> Member (view-only streams drop input/resize frames)
	go func() {
		for {
			typ, data, rerr := clientConn.Read(sctx)
			if rerr != nil {
				errCh <- rerr
				return
			}
			if view {
				continue
			}
			if werr := writeFrame(peerConn, typ, data); werr != nil {
				errCh <- werr
				return
			}
		}
	}()

	select {
	case <-sctx.Done():
	case <-errCh:
	}
}

// replicateWarrantAndRoster pushes the current DeputyWarrant and public member
// roster to all online Crew members over mTLS.
func (m *CrewManager) replicateWarrantAndRoster() {
	if m == nil || m.store == nil {
		return
	}
	st := m.store.StateSnapshot()
	if len(st.Members) == 0 {
		return
	}
	payload := crewWarrantSyncRequest{
		CrewID:  st.CrewID,
		Warrant: st.Warrant,
		Roster:  m.store.PublicRoster(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, mem := range st.Members {
		wg.Add(1)
		go func(mem CrewMember) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), m.pollTimeout)
			defer cancel()
			_, _, _ = m.doMemberRequest(ctx, mem, http.MethodPost, "/crew/v1/warrant", body, roleFull)
		}(mem)
	}
	wg.Wait()
}

// JoinUsingInvite consumes a `tmcrew1:<base64url>` invite token, sends a signed
// POST /crew/v1/join request to the Lead, and pins the Lead's certificate locally.
func (m *CrewManager) JoinUsingInvite(ctx context.Context, rawToken, memberName, memberURL string) (*crewJoinResponse, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("crew manager is not initialized")
	}
	tok, err := ParseInviteToken(rawToken)
	if err != nil {
		return nil, err
	}
	_, exp, err := parseCrewTimestamp(tok.ExpiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		return nil, errors.New("invite token has expired")
	}

	ident := m.store.Identity()
	name := strings.TrimSpace(memberName)
	if name == "" {
		if tok.Label != "" {
			name = tok.Label
		} else {
			name = ident.Name
		}
	}
	advURL := strings.TrimRight(strings.TrimSpace(memberURL), "/")
	if advURL == "" {
		advURL = m.store.StateSnapshot().AdvertiseURL
	}
	if advURL == "" {
		return nil, errors.New("member URL is required to join a crew")
	}
	if strings.HasPrefix(strings.ToLower(advURL), "http://") {
		advURL = "https://" + advURL[len("http://"):]
	}
	_ = m.store.SetNodeNameAndURL(name, advURL)
	ident = m.store.Identity()

	joinReq := crewJoinRequest{
		InviteID:   tok.InviteID,
		SecretHex:  tok.SecretHex,
		MemberID:   ident.ID,
		MemberName: ident.Name,
		MemberURL:  advURL,
		CertPEM:    ident.CertPEM,
		CertSha256: ident.CertSha256,
	}
	body, err := json.Marshal(joinReq)
	if err != nil {
		return nil, err
	}

	reqPath := "/crew/v1/join"
	ts := m.nextTimestampString()
	sig, err := signCrewRequest(ident.PrivateKey, http.MethodPost, reqPath, body, ts)
	if err != nil {
		return nil, err
	}

	leadBase := strings.TrimRight(strings.TrimSpace(tok.LeadURL), "/")
	if strings.HasPrefix(strings.ToLower(leadBase), "http://") {
		leadBase = "https://" + leadBase[len("http://"):]
	}
	fullURL := leadBase + reqPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(headerCrewTimestamp, ts)
	httpReq.Header.Set(headerCrewSignature, sig)
	httpReq.Header.Set(headerCrewSenderID, ident.ID)

	tr := &http.Transport{
		TLSClientConfig:   m.store.ClientTLSConfigForPeer(tok.LeadCertSha256),
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("contact lead %s: %w", tok.LeadURL, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxCrewControlBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var errObj struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBytes, &errObj) == nil && errObj.Error != "" {
			return nil, errors.New(errObj.Error)
		}
		return nil, fmt.Errorf("join rejected with HTTP %d", resp.StatusCode)
	}

	var joinResp crewJoinResponse
	if err := json.Unmarshal(respBytes, &joinResp); err != nil {
		return nil, fmt.Errorf("decode join response: %w", err)
	}
	if !constantTimeHexEqual(joinResp.LeadCertSha256, tok.LeadCertSha256) {
		return nil, errors.New("lead response certSha256 does not match invite pin")
	}
	if joinResp.LeadCertPEM != "" {
		if _, fp, _, perr := parseCertPEM(joinResp.LeadCertPEM); perr != nil || !constantTimeHexEqual(fp, tok.LeadCertSha256) {
			return nil, errors.New("lead response certificate PEM does not match invite pin")
		}
	} else {
		joinResp.LeadCertPEM = tok.LeadCertPEM
	}

	binding := CrewLeadBinding{
		CrewID:          joinResp.CrewID,
		LeadID:          joinResp.LeadID,
		LeadName:        joinResp.LeadName,
		LeadURL:         leadBase,
		LeadCertSha256:  tok.LeadCertSha256,
		LeadCertPEM:     joinResp.LeadCertPEM,
		SharedSecretHex: joinResp.CrewSecretHex,
		JoinedAt:        time.Now().UTC().Format(time.RFC3339),
		LastSeen:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.store.SetLeadBinding(binding); err != nil {
		return nil, err
	}
	if joinResp.Warrant != nil || len(joinResp.Roster) > 0 {
		_ = m.store.ApplyWarrantUpdate(joinResp.Warrant, joinResp.Roster, time.Now().UTC())
	}
	return &joinResp, nil
}

// Promote activates a warranted Standby Deputy as the active controller and
// immediately polls all replicated Crew members using the Lead-signed warrant.
func (m *CrewManager) Promote() error {
	if m == nil || m.store == nil {
		return errors.New("crew manager is not initialized")
	}
	if err := m.store.PromoteDeputy(time.Now().UTC()); err != nil {
		return err
	}
	m.PollOnce()
	return nil
}

// Status returns the full CrewStatusResponse for GET /api/mux/crew/status and
// `termote crew status`.
func (m *CrewManager) Status() CrewStatusResponse {
	ident := m.store.Identity()
	st := m.store.StateSnapshot()
	resp := CrewStatusResponse{
		CrewID:       st.CrewID,
		NodeID:       ident.ID,
		NodeName:     ident.Name,
		CertSha256:   ident.CertSha256,
		CertPEM:      ident.CertPEM,
		Role:         st.Role,
		AdvertiseURL: st.AdvertiseURL,
		DeputyID:     st.DeputyID,
		Warrant:      st.Warrant,
		Members:      []CrewMemberStatus{},
		Roster:       m.store.PublicRoster(),
	}
	if st.Lead != nil {
		resp.Lead = &CrewLeadPublic{
			CrewID:         st.Lead.CrewID,
			LeadID:         st.Lead.LeadID,
			LeadName:       st.Lead.LeadName,
			LeadURL:        st.Lead.LeadURL,
			LeadCertSha256: st.Lead.LeadCertSha256,
			JoinedAt:       st.Lead.JoinedAt,
			LastSeen:       st.Lead.LastSeen,
		}
	}

	m.mu.RLock()
	for _, mem := range st.Members {
		roleName := mem.Role
		if st.DeputyID != "" && mem.ID == st.DeputyID {
			roleName = CrewRoleDeputy
		} else if roleName == "" {
			roleName = CrewRoleMember
		}
		if entry, ok := m.cache[mem.ID]; ok {
			item := entry.status
			item.Role = roleName
			resp.Members = append(resp.Members, item)
		} else {
			resp.Members = append(resp.Members, CrewMemberStatus{
				ID:         mem.ID,
				Name:       mem.Name,
				URL:        mem.URL,
				CertSha256: mem.CertSha256,
				Role:       roleName,
				Status:     "unreachable",
				LastSeen:   mem.LastSeen,
			})
		}
	}
	m.mu.RUnlock()
	return resp
}

func (m *CrewManager) registerAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mux/crew/status", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if msg := crossSiteRejection(m.allowed, r); msg != "" {
			jsonError(w, msg, http.StatusForbidden)
			return
		}
		if !requireWriteRole(w, r) {
			return
		}
		jsonOK(w, m.Status())
	})

	mux.HandleFunc("/api/mux/crew/invite", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		var body struct {
			Name    string `json:"name"`
			LeadURL string `json:"leadUrl"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		leadURL := strings.TrimSpace(body.LeadURL)
		if leadURL == "" {
			leadURL = m.store.StateSnapshot().AdvertiseURL
		}
		if leadURL == "" && r.Host != "" && r.Host != localSocketHost {
			leadURL = "https://" + r.Host
		}
		tokStr, tok, err := m.store.CreateInvite(leadURL, body.Name, defaultCrewInviteTTL, time.Now().UTC())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOK(w, map[string]any{
			"ok":        true,
			"invite":    tokStr,
			"inviteId":  tok.InviteID,
			"expiresAt": tok.ExpiresAt,
			"leadUrl":   tok.LeadURL,
		})
	})

	mux.HandleFunc("/api/mux/crew/join", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		var body struct {
			Invite    string `json:"invite"`
			Name      string `json:"name"`
			MemberURL string `json:"memberUrl"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		memberURL := strings.TrimSpace(body.MemberURL)
		if memberURL == "" && r.Host != "" && r.Host != localSocketHost {
			memberURL = "https://" + r.Host
		}
		resp, err := m.JoinUsingInvite(r.Context(), body.Invite, body.Name, memberURL)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOK(w, resp)
	})

	mux.HandleFunc("/api/mux/crew/add", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		var body struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			URL        string `json:"url"`
			CertSha256 string `json:"certSha256"`
			CertPEM    string `json:"certPem"`
			SecretHex  string `json:"secretHex"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		mem := CrewMember{
			ID:              body.ID,
			Name:            body.Name,
			URL:             body.URL,
			CertSha256:      body.CertSha256,
			CertPEM:         body.CertPEM,
			SharedSecretHex: body.SecretHex,
			Role:            CrewRoleMember,
		}
		if err := m.store.AddOrUpdateMember(mem); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.replicateWarrantAndRoster()
		m.PollOnce()
		jsonOK(w, map[string]any{"ok": true, "status": m.Status()})
	})

	mux.HandleFunc("/api/mux/crew/remove", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		var body struct {
			MemberID string `json:"memberId"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		removed, ok, err := m.store.RemoveMember(body.MemberID)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			jsonError(w, fmt.Sprintf("member %q not found", body.MemberID), http.StatusNotFound)
			return
		}
		m.removeMemberCache(removed.ID)
		m.replicateWarrantAndRoster()
		jsonOK(w, map[string]any{"ok": true, "removed": removed.ID})
	})

	mux.HandleFunc("/api/mux/crew/deputy", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		var body struct {
			MemberID string `json:"memberId"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		warrant, err := m.store.DesignateDeputy(body.MemberID, time.Now().UTC())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.replicateWarrantAndRoster()
		jsonOK(w, map[string]any{"ok": true, "warrant": warrant, "deputyId": m.store.StateSnapshot().DeputyID})
	})

	mux.HandleFunc("/api/mux/crew/promote", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) || !requireWriteRole(w, r) {
			return
		}
		if err := m.Promote(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonOK(w, map[string]any{"ok": true, "role": m.store.Role(), "status": m.Status()})
	})
}
