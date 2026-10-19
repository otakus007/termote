package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type crewTestMux struct {
	mu     sync.Mutex
	snap   Snapshot
	calls  []string
	attach func(pane string, size Size) (TermStream, error)
}

func (m *crewTestMux) Name() string { return "tmux" }
func (m *crewTestMux) Caps() Caps {
	return Caps{CopyMode: true, Groups: true, Files: true}
}
func (m *crewTestMux) Snapshot(context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "snapshot")
	return m.snap, nil
}
func (m *crewTestMux) SelectTab(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "select "+id)
	return nil
}
func (m *crewTestMux) NewTab(_ context.Context, group, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "new "+group+"/"+name)
	return "tab-new", nil
}
func (m *crewTestMux) CloseTab(_ context.Context, id, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "close "+id+" "+key)
	return nil
}
func (m *crewTestMux) RenameTab(_ context.Context, id, name, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "rename "+id+"="+name)
	return nil
}
func (m *crewTestMux) MoveTab(_ context.Context, id string, _ int) (string, error) {
	return id, nil
}
func (m *crewTestMux) MoveGroup(_ context.Context, _ string, _ int) error { return nil }
func (m *crewTestMux) NewGroup(_ context.Context, name, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "new group "+name)
	return "grp-new", nil
}
func (m *crewTestMux) CloseGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "close group "+id)
	return nil
}
func (m *crewTestMux) RenameGroup(_ context.Context, id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "rename group "+id+"="+name)
	return nil
}
func (m *crewTestMux) ClosePane(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "close pane "+id)
	return nil
}
func (m *crewTestMux) SendKeys(_ context.Context, id, keys string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "keys "+id+":"+keys)
	return nil
}
func (m *crewTestMux) Scroll(_ context.Context, id string, lines int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "scroll "+id)
	return nil
}
func (m *crewTestMux) ReadText(_ context.Context, _ string, _ int, w io.Writer) (bool, error) {
	_, _ = w.Write([]byte("pane text"))
	return false, nil
}
func (m *crewTestMux) Attach(_ context.Context, pane string, size Size) (TermStream, error) {
	if m.attach != nil {
		return m.attach(pane, size)
	}
	return nil, errUnsupported
}
func (m *crewTestMux) Health(context.Context) error { return nil }
func (m *crewTestMux) recordedCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

type crewTestNode struct {
	name    string
	dir     string
	mux     *crewTestMux
	crew    *CrewManager
	machine *machineCollector
	server  *httptest.Server
}

func newCrewTestNode(t *testing.T, name string, snap Snapshot) *crewTestNode {
	t.Helper()
	dir := t.TempDir()
	mux := &crewTestMux{snap: snap}
	cfg := serveConfig{
		Port:        "0",
		User:        "admin",
		Pass:        "secret-pass-123",
		NoAuth:      false,
		Bind:        "127.0.0.1",
		MuxBackend:  "tmux",
		InstanceDir: dir,
		CrewDir:     filepath.Join(dir, "crew"),
		MachineDir:  filepath.Join(dir, "machine"),
		DevicesDir:  filepath.Join(dir, "devices"),
		NodeName:    name,
	}
	handler, _, _, _, machine, crew, err := buildServerWithCrew(cfg, mux)
	if err != nil {
		t.Fatalf("buildServerWithCrew(%s): %v", name, err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = newTLSOrHTTPListener(srv.Listener, crew.Store().PeerTLSConfig())
	srv.Start()
	httpsURL := "https://" + strings.TrimPrefix(srv.URL, "http://")
	_ = crew.Store().SetNodeNameAndURL(name, httpsURL)
	srv.URL = httpsURL
	t.Cleanup(func() {
		crew.Close()
		srv.Close()
	})
	return &crewTestNode{
		name:    name,
		dir:     dir,
		mux:     mux,
		crew:    crew,
		machine: machine,
		server:  srv,
	}
}

func TestCrewInviteSingleUseAndExpiry(t *testing.T) {
	lead := newCrewTestNode(t, "lead-box", Snapshot{
		Groups: []Group{{ID: "g-lead", Name: "lead-group", Tabs: []Tab{{ID: "0", Key: "@0", Name: "main", Active: true, Panes: []Pane{{ID: "%0", Active: true}}}}}},
	})
	member1 := newCrewTestNode(t, "worker-1", Snapshot{
		Groups: []Group{{ID: "g-w1", Name: "worker-group", Tabs: []Tab{{ID: "0", Key: "@1", Name: "code", Active: true, Panes: []Pane{{ID: "%1", Active: true}}}}}},
	})
	member2 := newCrewTestNode(t, "worker-2", Snapshot{})

	// 1. Mint a 10m single-use invite on Lead.
	tokStr, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "worker-1", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// 2. First join succeeds.
	joinResp, err := member1.crew.JoinUsingInvite(context.Background(), tokStr, "worker-1", member1.server.URL)
	if err != nil {
		t.Fatalf("first JoinUsingInvite failed: %v", err)
	}
	if !joinResp.OK || joinResp.CrewID == "" {
		t.Fatalf("unexpected joinResp: %+v", joinResp)
	}

	// 3. Reusing the same single-use invite token on another node must be rejected.
	if _, err := member2.crew.JoinUsingInvite(context.Background(), tokStr, "worker-2", member2.server.URL); err == nil {
		t.Fatalf("expected second join with single-use invite to fail")
	}

	// 4. An expired invite token must be rejected.
	expiredTok, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "expired", 1*time.Second, time.Now().UTC().Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("CreateInvite expired: %v", err)
	}
	if _, err := member2.crew.JoinUsingInvite(context.Background(), expiredTok, "worker-2", member2.server.URL); err == nil {
		t.Fatalf("expected expired invite to fail")
	}
}

func TestCrewRejectsUnpinnedCertificateAndReplay(t *testing.T) {
	lead := newCrewTestNode(t, "lead-box", Snapshot{})
	member := newCrewTestNode(t, "worker-1", Snapshot{
		Groups: []Group{{ID: "g1", Name: "w1", Tabs: []Tab{{ID: "0", Key: "@0", Name: "t0", Panes: []Pane{{ID: "%0", Active: true}}}}}},
	})
	intruder := newCrewTestNode(t, "intruder-box", Snapshot{})

	tokStr, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "worker-1", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := member.crew.JoinUsingInvite(context.Background(), tokStr, "worker-1", member.server.URL); err != nil {
		t.Fatalf("JoinUsingInvite: %v", err)
	}

	// 1. Intruder with an unpinned ECDSA P-256 certificate attempting to poll Member's
	// /crew/v1/snapshot must be rejected at the mTLS handshake.
	intruderClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				Certificates:       []tls.Certificate{intruder.crew.Store().Identity().TLSCert},
				InsecureSkipVerify: true,
			},
		},
	}
	req, _ := http.NewRequest(http.MethodGet, member.server.URL+"/crew/v1/snapshot", nil)
	if resp, err := intruderClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected unpinned certificate request to fail, got 200 OK")
		}
	}

	// 2. Replay attack: sending two requests from Lead to Member with the exact same
	// X-Crew-Timestamp + X-Crew-Signature must fail on the second attempt.
	memRecord, ok := lead.crew.Store().GetMember(member.crew.Store().Identity().ID)
	if !ok {
		t.Fatalf("member not registered on lead")
	}
	leadIdent := lead.crew.Store().Identity()
	ts := lead.crew.nextTimestampString()
	reqPath := "/crew/v1/snapshot"
	sig, err := signCrewRequest(leadIdent.PrivateKey, http.MethodGet, reqPath, nil, ts)
	if err != nil {
		t.Fatalf("signCrewRequest: %v", err)
	}
	macHex, err := computeCrewAuthHMAC(memRecord.SharedSecretHex, http.MethodGet, reqPath, nil, ts)
	if err != nil {
		t.Fatalf("computeCrewAuthHMAC: %v", err)
	}

	leadTLSClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: lead.crew.Store().ClientTLSConfigForPeer(memRecord.CertSha256),
		},
	}
	doSigned := func() int {
		r, _ := http.NewRequest(http.MethodGet, member.server.URL+reqPath, nil)
		r.Header.Set(headerCrewTimestamp, ts)
		r.Header.Set(headerCrewSignature, sig)
		r.Header.Set(headerCrewSenderID, leadIdent.ID)
		r.Header.Set(headerCrewAuthHMAC, macHex)
		resp, err := leadTLSClient.Do(r)
		if err != nil {
			t.Fatalf("doSigned: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := doSigned(); code != http.StatusOK {
		t.Fatalf("first signed request status = %d, want 200", code)
	}
	if code := doSigned(); code != http.StatusUnauthorized {
		t.Fatalf("replayed signed request status = %d, want 401", code)
	}
}

func TestCrewAggregationProxyAndViewRoleEnforcement(t *testing.T) {
	lead := newCrewTestNode(t, "lead-box", Snapshot{
		Groups: []Group{{
			ID:   "local-main",
			Name: "local-main",
			Tabs: []Tab{{ID: "0", Key: "@0", Name: "shell", Active: true, Panes: []Pane{{ID: "%0", Active: true, Title: "lead-pane", Process: &ProcessInfo{Name: "bash", Cwd: "/home/lead/work"}}}}},
		}},
	})
	member := newCrewTestNode(t, "gpu-box", Snapshot{
		Groups: []Group{{
			ID:   "remote-train",
			Name: "remote-train",
			Tabs: []Tab{{ID: "0", Key: "@9", Name: "trainer", Active: true, Panes: []Pane{{ID: "%7", Active: true, Title: "gpu-pane", Process: &ProcessInfo{Name: "python", Cwd: "/home/gpu/secret-model"}}}}},
		}},
	})

	tokStr, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "gpu-box", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := member.crew.JoinUsingInvite(context.Background(), tokStr, "gpu-box", member.server.URL); err != nil {
		t.Fatalf("JoinUsingInvite: %v", err)
	}
	lead.crew.PollOnce()

	// 0. Plain HTTP front-door request on the same multiplexed listener works alongside TLS 1.3 mTLS.
	plainHTTPURL := "http://" + strings.TrimPrefix(lead.server.URL, "https://")
	plainReq, _ := http.NewRequest(http.MethodGet, plainHTTPURL+"/api/mux/health", nil)
	plainReq.SetBasicAuth("admin", "secret-pass-123")
	plainResp, err := http.DefaultClient.Do(plainReq)
	if err != nil {
		t.Fatalf("plain HTTP GET /api/mux/health on multiplexed listener failed: %v", err)
	}
	plainResp.Body.Close()
	if plainResp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP health status = %d, want 200", plainResp.StatusCode)
	}

	memberID := member.crew.Store().Identity().ID
	authedClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// 1. Verify merged /api/mux/snapshot has local + c:<memberId>/ namespaced group and crewMembers[].
	req, _ := http.NewRequest(http.MethodGet, lead.server.URL+"/api/mux/snapshot", nil)
	req.SetBasicAuth("admin", "secret-pass-123")
	resp, err := authedClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/mux/snapshot: %v", err)
	}
	var merged Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&merged); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	resp.Body.Close()

	if len(merged.CrewMembers) != 2 {
		t.Fatalf("len(CrewMembers) = %d, want 2 (%+v)", len(merged.CrewMembers), merged.CrewMembers)
	}
	if len(merged.Groups) != 2 {
		t.Fatalf("len(Groups) = %d, want 2 (%+v)", len(merged.Groups), merged.Groups)
	}
	remoteGrp := merged.Groups[1]
	wantGroupID := "c:" + memberID + "/remote-train"
	wantPaneID := "c:" + memberID + "/%7"
	if remoteGrp.ID != wantGroupID || remoteGrp.MachineID != memberID || remoteGrp.MachineName != "gpu-box" {
		t.Fatalf("remote group = %+v, want ID=%s MachineID=%s MachineName=gpu-box", remoteGrp, wantGroupID, memberID)
	}
	if gotPane := remoteGrp.Tabs[0].Panes[0].ID; gotPane != wantPaneID {
		t.Fatalf("remote pane ID = %q, want %q", gotPane, wantPaneID)
	}
	if remoteGrp.Tabs[0].Panes[0].Process == nil || remoteGrp.Tabs[0].Panes[0].Process.Cwd != "/home/gpu/secret-model" {
		t.Fatalf("expected full caller to see remote Process.Cwd, got %+v", remoteGrp.Tabs[0].Panes[0].Process)
	}

	// 1b. Cross-site GET to proxied member pane route is rejected with 403 Forbidden.
	csReq, _ := http.NewRequest(http.MethodGet, lead.server.URL+"/api/mux/panes/c:"+memberID+"/%257/text", nil)
	csReq.SetBasicAuth("admin", "secret-pass-123")
	csReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csResp, err := authedClient.Do(csReq)
	if err != nil {
		t.Fatalf("cross-site GET: %v", err)
	}
	csResp.Body.Close()
	if csResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site GET status = %d, want 403 Forbidden", csResp.StatusCode)
	}

	// 1c. View-only snapshot strips Process.Cwd on both local and federated panes without mutating cache.
	viewSnap := lead.crew.AugmentSnapshot(lead.mux.snap)
	stripSnapshotDirs(&viewSnap)
	for _, g := range viewSnap.Groups {
		for _, tb := range g.Tabs {
			for _, pn := range tb.Panes {
				if pn.Process != nil && pn.Process.Cwd != "" {
					t.Fatalf("expected view snapshot to strip Cwd on pane %s, got %q", pn.ID, pn.Process.Cwd)
				}
			}
		}
	}
	// Cached member snapshot must retain its Cwd for subsequent full callers.
	fullAfter := lead.crew.AugmentSnapshot(lead.mux.snap)
	if fullAfter.Groups[1].Tabs[0].Panes[0].Process == nil || fullAfter.Groups[1].Tabs[0].Panes[0].Process.Cwd != "/home/gpu/secret-model" {
		t.Fatalf("stripSnapshotDirs mutated cached member snapshot Cwd: %+v", fullAfter.Groups[1].Tabs[0].Panes[0].Process)
	}

	// 2. Proxy a full-role write (POST /api/mux/panes/c:<memberId>/%257/keys) through Lead to Member.
	keysBody := []byte(`{"keys":"ls -la\n"}`)
	keysURL := lead.server.URL + "/api/mux/panes/c:" + memberID + "/%257/keys"
	kreq, _ := http.NewRequest(http.MethodPost, keysURL, bytes.NewReader(keysBody))
	kreq.SetBasicAuth("admin", "secret-pass-123")
	kreq.Header.Set("Content-Type", "application/json")
	kresp, err := authedClient.Do(kreq)
	if err != nil {
		t.Fatalf("POST proxied keys: %v", err)
	}
	kresp.Body.Close()
	if kresp.StatusCode != http.StatusOK {
		t.Fatalf("POST proxied keys status = %d, want 200", kresp.StatusCode)
	}
	if !slices.Contains(member.mux.recordedCalls(), "keys %7:ls -la\n") {
		t.Fatalf("member mux did not record stripped pane ID keys call: %v", member.mux.recordedCalls())
	}

	// 3. R5.5: Verify a view-only client cannot execute writes on the Member (neither through Lead
	// nor when X-Termote-Client-Role: view is propagated to Member).
	memRecord, _ := lead.crew.Store().GetMember(memberID)
	pResp, _, err := lead.crew.doMemberRequest(
		context.Background(),
		memRecord,
		http.MethodPost,
		"/crew/v1/proxy/api/mux/panes/%257/keys",
		keysBody,
		roleView,
	)
	if err != nil {
		t.Fatalf("doMemberRequest with roleView: %v", err)
	}
	if pResp.StatusCode != http.StatusForbidden {
		t.Fatalf("member proxy with X-Termote-Client-Role=view returned %d, want 403 Forbidden", pResp.StatusCode)
	}

	// 4. Verify GET /api/mux/machine?machine=<memberId> returns Member's telemetry and Machines[] list.
	mreq, _ := http.NewRequest(http.MethodGet, lead.server.URL+"/api/mux/machine?machine="+memberID, nil)
	mreq.SetBasicAuth("admin", "secret-pass-123")
	mresp, err := authedClient.Do(mreq)
	if err != nil {
		t.Fatalf("GET /api/mux/machine: %v", err)
	}
	var mstatus MachineStatusResponse
	if err := json.NewDecoder(mresp.Body).Decode(&mstatus); err != nil {
		t.Fatalf("decode machine status: %v", err)
	}
	mresp.Body.Close()
	if mstatus.Host.ID != memberID || mstatus.Host.Name != "gpu-box" {
		t.Fatalf("machine host = %+v, want ID=%s Name=gpu-box", mstatus.Host, memberID)
	}
	if len(mstatus.Machines) != 2 {
		t.Fatalf("len(mstatus.Machines) = %d, want 2", len(mstatus.Machines))
	}
}

func TestCrewOfflineMemberFastSnapshot(t *testing.T) {
	lead := newCrewTestNode(t, "lead-box", Snapshot{
		Groups: []Group{{ID: "g0", Name: "local", Tabs: []Tab{{ID: "0", Key: "@0", Name: "sh", Panes: []Pane{{ID: "%0", Active: true}}}}}},
	})
	member := newCrewTestNode(t, "offline-box", Snapshot{
		Groups: []Group{{ID: "g1", Name: "remote", Tabs: []Tab{{ID: "0", Key: "@1", Name: "sh", Panes: []Pane{{ID: "%1", Active: true}}}}}},
	})

	tokStr, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "offline-box", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := member.crew.JoinUsingInvite(context.Background(), tokStr, "offline-box", member.server.URL); err != nil {
		t.Fatalf("JoinUsingInvite: %v", err)
	}
	lead.crew.PollOnce()

	// Shut down Member and refresh poller once so Member is marked unreachable.
	member.server.Close()
	lead.crew.PollOnce()

	authedClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Lead snapshot must return in < 50ms and report member status == "unreachable".
	t0 := time.Now()
	req, _ := http.NewRequest(http.MethodGet, lead.server.URL+"/api/mux/snapshot", nil)
	req.SetBasicAuth("admin", "secret-pass-123")
	resp, err := authedClient.Do(req)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("GET /api/mux/snapshot: %v", err)
	}
	var snap Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()

	if elapsed > 50*time.Millisecond {
		t.Fatalf("Lead snapshot with offline member took %s, want < 50ms", elapsed)
	}
	if len(snap.Groups) != 1 {
		t.Fatalf("len(snap.Groups) = %d, want 1 (local only)", len(snap.Groups))
	}
	var foundOffline bool
	for _, cm := range snap.CrewMembers {
		if cm.Name == "offline-box" && cm.Status == "unreachable" {
			foundOffline = true
		}
	}
	if !foundOffline {
		t.Fatalf("expected offline-box with status=unreachable in CrewMembers: %+v", snap.CrewMembers)
	}
}

func TestCrewDeputyWarrantFailover(t *testing.T) {
	lead := newCrewTestNode(t, "lead-box", Snapshot{
		Groups: []Group{{ID: "g-lead", Name: "lead", Tabs: []Tab{{ID: "0", Key: "@0", Name: "t", Panes: []Pane{{ID: "%0", Active: true}}}}}},
	})
	deputy := newCrewTestNode(t, "deputy-box", Snapshot{
		Groups: []Group{{ID: "g-dep", Name: "deputy", Tabs: []Tab{{ID: "0", Key: "@1", Name: "t", Panes: []Pane{{ID: "%1", Active: true}}}}}},
	})
	worker := newCrewTestNode(t, "worker-box", Snapshot{
		Groups: []Group{{ID: "g-wrk", Name: "worker", Tabs: []Tab{{ID: "0", Key: "@2", Name: "t", Panes: []Pane{{ID: "%2", Active: true}}}}}},
	})

	// Join both Deputy and Worker to Lead.
	inv1, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "deputy-box", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite 1: %v", err)
	}
	if _, err := deputy.crew.JoinUsingInvite(context.Background(), inv1, "deputy-box", deputy.server.URL); err != nil {
		t.Fatalf("deputy JoinUsingInvite: %v", err)
	}

	inv2, _, err := lead.crew.Store().CreateInvite(lead.server.URL, "worker-box", defaultCrewInviteTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateInvite 2: %v", err)
	}
	if _, err := worker.crew.JoinUsingInvite(context.Background(), inv2, "worker-box", worker.server.URL); err != nil {
		t.Fatalf("worker JoinUsingInvite: %v", err)
	}

	// Designate deputy-box as Standby Deputy on Lead and replicate the Lead-signed DeputyWarrant + Roster.
	deputyID := deputy.crew.Store().Identity().ID
	workerID := worker.crew.Store().Identity().ID
	warrant, err := lead.crew.Store().DesignateDeputy(deputyID, time.Now().UTC())
	if err != nil || warrant == nil {
		t.Fatalf("DesignateDeputy: %v", err)
	}
	lead.crew.replicateWarrantAndRoster()

	if gotRole := deputy.crew.Store().Role(); gotRole != CrewRoleDeputy {
		t.Fatalf("deputy role = %q, want %q", gotRole, CrewRoleDeputy)
	}

	// Simulate Lead failure and promote Deputy.
	lead.server.Close()
	if err := deputy.crew.Promote(); err != nil {
		t.Fatalf("deputy.Promote(): %v", err)
	}
	if gotRole := deputy.crew.Store().Role(); gotRole != CrewRoleLead {
		t.Fatalf("promoted deputy role = %q, want %q", gotRole, CrewRoleLead)
	}

	// Promoted Deputy must now aggregate Worker's groups under `c:<workerId>/g-wrk`
	// because Worker verifies the Lead-signed DeputyWarrant presented by Deputy.
	merged := deputy.crew.AugmentSnapshot(deputy.mux.snap)
	var foundWorkerGroup bool
	wantWorkerGroupID := "c:" + workerID + "/g-wrk"
	for _, g := range merged.Groups {
		if g.ID == wantWorkerGroupID {
			foundWorkerGroup = true
		}
	}
	if !foundWorkerGroup {
		t.Fatalf("promoted deputy did not aggregate worker group %q in %+v", wantWorkerGroupID, merged.Groups)
	}

	// Proxy a command from Promoted Deputy to Worker.
	workerMem, ok := deputy.crew.Store().GetMember(workerID)
	if !ok {
		t.Fatalf("worker not in promoted deputy members")
	}
	resp, _, err := deputy.crew.doMemberRequest(
		context.Background(),
		workerMem,
		http.MethodPost,
		"/crew/v1/proxy/api/mux/panes/%252/keys",
		[]byte(`{"keys":"echo failover\n"}`),
		roleFull,
	)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("promoted deputy proxy to worker failed: err=%v status=%v", err, resp)
	}
	if !slices.Contains(worker.mux.recordedCalls(), "keys %2:echo failover\n") {
		t.Fatalf("worker did not execute command from promoted deputy: %v", worker.mux.recordedCalls())
	}

	// Verify an unwarranted node (Worker itself) cannot self-promote.
	if err := worker.crew.Promote(); err == nil || !strings.Contains(err.Error(), "cannot promote") {
		t.Fatalf("expected unwarranted worker Promote() to fail, got %v", err)
	}
}
