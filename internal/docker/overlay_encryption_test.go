package docker

import (
	"context"
	"errors"
	"testing"
)

// netFakeClient extends the basic fakeClient with canned answers for
// SelfAttachedNetworks and NetworkInspect, since the policy check
// composes both.
type netFakeClient struct {
	fakeClient

	networks    []string
	networksErr error
	inspect     map[string]NetworkSummary
	inspectErr  map[string]error
}

func (n *netFakeClient) SelfAttachedNetworks(context.Context) ([]string, error) {
	return n.networks, n.networksErr
}

func (n *netFakeClient) NetworkInspect(_ context.Context, name string) (NetworkSummary, error) {
	if err, ok := n.inspectErr[name]; ok {
		return NetworkSummary{}, err
	}
	return n.inspect[name], nil
}

func TestCheckOverlayEncryption_AllEncryptedOverlaysPass(t *testing.T) {
	c := &netFakeClient{
		networks: []string{"coord-net"},
		inspect: map[string]NetworkSummary{
			"coord-net": {Name: "coord-net", Driver: "overlay", Encrypted: true},
		},
	}
	if err := CheckOverlayEncryption(context.Background(), c); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestCheckOverlayEncryption_NonOverlayIgnored(t *testing.T) {
	// Bridge, host, none are local-only — never on the wire between hosts.
	// The check must not fail on them.
	c := &netFakeClient{
		networks: []string{"bridge", "host", "none"},
		inspect: map[string]NetworkSummary{
			"bridge": {Name: "bridge", Driver: "bridge", Encrypted: false},
			"host":   {Name: "host", Driver: "host", Encrypted: false},
			"none":   {Name: "none", Driver: "null", Encrypted: false},
		},
	}
	if err := CheckOverlayEncryption(context.Background(), c); err != nil {
		t.Errorf("err = %v, want nil (non-overlay networks must be ignored)", err)
	}
}

func TestCheckOverlayEncryption_UnencryptedOverlayFails(t *testing.T) {
	c := &netFakeClient{
		networks: []string{"coord-net"},
		inspect: map[string]NetworkSummary{
			"coord-net": {Name: "coord-net", Driver: "overlay", Encrypted: false},
		},
	}
	err := CheckOverlayEncryption(context.Background(), c)
	if !errors.Is(err, ErrUnencryptedOverlay) {
		t.Errorf("err = %v, want errors.Is(_, ErrUnencryptedOverlay)", err)
	}
	if err == nil || !contains(err.Error(), "coord-net") {
		t.Errorf("err = %v, want it to name the offending network", err)
	}
}

func TestCheckOverlayEncryption_IngressDetected(t *testing.T) {
	// The `ingress` network is Swarm's load-balancer overlay; it's
	// attached automatically when a service has `published:` ports. It's
	// not encrypted by default and the operator can't easily change that.
	// Our policy: if it shows up, fail — they shouldn't be publishing
	// ports on the coordinator anyway under the new design.
	c := &netFakeClient{
		networks: []string{"ingress", "coord-net"},
		inspect: map[string]NetworkSummary{
			"ingress":   {Name: "ingress", Driver: "overlay", Encrypted: false},
			"coord-net": {Name: "coord-net", Driver: "overlay", Encrypted: true},
		},
	}
	err := CheckOverlayEncryption(context.Background(), c)
	if !errors.Is(err, ErrUnencryptedOverlay) {
		t.Errorf("err = %v, want ErrUnencryptedOverlay", err)
	}
	if err == nil || !contains(err.Error(), "ingress") {
		t.Errorf("err = %v, want it to call out ingress specifically", err)
	}
}

func TestCheckOverlayEncryption_MultipleUnencryptedReportedSorted(t *testing.T) {
	// Sorted output so log lines are stable across runs / restarts.
	c := &netFakeClient{
		networks: []string{"zzz-net", "aaa-net"},
		inspect: map[string]NetworkSummary{
			"zzz-net": {Name: "zzz-net", Driver: "overlay", Encrypted: false},
			"aaa-net": {Name: "aaa-net", Driver: "overlay", Encrypted: false},
		},
	}
	err := CheckOverlayEncryption(context.Background(), c)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	a := indexOf(msg, "aaa-net")
	z := indexOf(msg, "zzz-net")
	if a < 0 || z < 0 || a > z {
		t.Errorf("offending networks not sorted in %q", msg)
	}
}

func TestCheckOverlayEncryption_ListErrorPropagates(t *testing.T) {
	c := &netFakeClient{networksErr: errors.New("docker socket down")}
	err := CheckOverlayEncryption(context.Background(), c)
	if err == nil {
		t.Fatal("expected error on list-networks failure")
	}
	if errors.Is(err, ErrUnencryptedOverlay) {
		t.Errorf("err = %v, want underlying error not ErrUnencryptedOverlay", err)
	}
}

func TestCheckOverlayEncryption_InspectErrorPropagates(t *testing.T) {
	c := &netFakeClient{
		networks:   []string{"coord-net"},
		inspectErr: map[string]error{"coord-net": errors.New("transient")},
	}
	err := CheckOverlayEncryption(context.Background(), c)
	if err == nil {
		t.Fatal("expected error on inspect failure")
	}
	if errors.Is(err, ErrUnencryptedOverlay) {
		t.Errorf("err = %v, want underlying error not ErrUnencryptedOverlay", err)
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
