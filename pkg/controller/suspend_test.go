package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"unifi-port-forward/pkg/api/v1alpha1"
	"unifi-port-forward/pkg/config"
)

const (
	gameNamespace = "palworld"
	gameService   = "palworld-peanut"
	ruleNamespace = "pf"
)

func suspendScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, discoveryv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}

func newSuspendReconciler(t *testing.T, grace time.Duration, objs ...client.Object) *PortForwardRuleReconciler {
	t.Helper()
	scheme := suspendScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&v1alpha1.PortForwardRule{}, serviceRefIndex, indexServiceRef).
		Build()
	return &PortForwardRuleReconciler{
		Client:   c,
		Scheme:   scheme,
		Config:   &config.Config{SuspendGrace: grace},
		Recorder: &record.FakeRecorder{},
	}
}

// suspendRule is a rule in the pf namespace pointing across namespaces at the
// game's Service, which is how the homelab lays them out.
func suspendRule(name string, optIn bool) *v1alpha1.PortForwardRule {
	ns := gameNamespace
	return &v1alpha1.PortForwardRule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ruleNamespace},
		Spec: v1alpha1.PortForwardRuleSpec{
			ExternalPort:           intstr.FromInt32(8211),
			Protocol:               "udp",
			Enabled:                true,
			ServiceRef:             &v1alpha1.ServiceReference{Name: gameService, Namespace: &ns, Port: "game"},
			SuspendWhenNoEndpoints: optIn,
		},
	}
}

func endpointSlice(name string, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: gameNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: gameService},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

func endpoint(ready bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{"10.42.0.53"},
		Conditions: discoveryv1.EndpointConditions{Ready: &ready},
	}
}

func TestEvaluateSuspensionStartsGraceWhenEndpointsVanish(t *testing.T) {
	r := newSuspendReconciler(t, 2*time.Minute, endpointSlice("empty"))
	rule := suspendRule("game", true)
	now := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)

	got, err := r.evaluateSuspension(context.Background(), rule, now)
	if err != nil {
		t.Fatalf("evaluateSuspension: %v", err)
	}
	if got.Suspended {
		t.Fatal("suspended immediately; want to wait out the grace period")
	}
	if got.RecheckAfter != 2*time.Minute {
		t.Errorf("RecheckAfter = %s, want 2m", got.RecheckAfter)
	}
	if rule.Status.NoEndpointsSince == nil || !rule.Status.NoEndpointsSince.Time.Equal(now) {
		t.Errorf("NoEndpointsSince = %v, want %s", rule.Status.NoEndpointsSince, now)
	}
}

func TestEvaluateSuspensionSuspendsAfterGrace(t *testing.T) {
	r := newSuspendReconciler(t, 2*time.Minute)
	rule := suspendRule("game", true)
	since := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	rule.Status.NoEndpointsSince = &metav1.Time{Time: since}

	got, err := r.evaluateSuspension(context.Background(), rule, since.Add(90*time.Second))
	if err != nil {
		t.Fatalf("evaluateSuspension: %v", err)
	}
	if got.Suspended || got.RecheckAfter != 30*time.Second {
		t.Fatalf("at 90s got %+v, want pending with 30s left", got)
	}

	got, err = r.evaluateSuspension(context.Background(), rule, since.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("evaluateSuspension: %v", err)
	}
	if !got.Suspended {
		t.Fatalf("at 2m got %+v, want suspended", got)
	}
	if !rule.Status.NoEndpointsSince.Time.Equal(since) {
		t.Error("NoEndpointsSince moved; the grace period must run from the first observation")
	}
}

func TestEvaluateSuspensionCountsNotReadyEndpoints(t *testing.T) {
	// A pod that is still booting (or updating, or crash-looping) exists, so its
	// port stays open. Only the absence of any pod suspends.
	r := newSuspendReconciler(t, time.Minute, endpointSlice("booting", endpoint(false)))
	rule := suspendRule("game", true)
	rule.Status.NoEndpointsSince = &metav1.Time{Time: time.Now().Add(-time.Hour)}

	got, err := r.evaluateSuspension(context.Background(), rule, time.Now())
	if err != nil {
		t.Fatalf("evaluateSuspension: %v", err)
	}
	if got.Suspended {
		t.Fatal("suspended while a not-ready endpoint exists")
	}
	if rule.Status.NoEndpointsSince != nil {
		t.Error("NoEndpointsSince not cleared once an endpoint appeared")
	}
}

func TestEvaluateSuspensionIgnoresRulesThatDidNotOptIn(t *testing.T) {
	r := newSuspendReconciler(t, time.Minute)
	rule := suspendRule("game", false)
	rule.Status.NoEndpointsSince = &metav1.Time{Time: time.Now().Add(-time.Hour)}

	got, err := r.evaluateSuspension(context.Background(), rule, time.Now())
	if err != nil {
		t.Fatalf("evaluateSuspension: %v", err)
	}
	if got.Suspended || got.RecheckAfter != 0 {
		t.Fatalf("got %+v for a rule without suspendWhenNoEndpoints", got)
	}
	if rule.Status.NoEndpointsSince != nil {
		t.Error("stale NoEndpointsSince kept after opting out")
	}
}

func TestRulesForEndpointSliceMapsAcrossNamespaces(t *testing.T) {
	optedIn := suspendRule("game", true)
	optedOut := suspendRule("query", false)
	r := newSuspendReconciler(t, time.Minute, optedIn, optedOut)

	requests := r.rulesForEndpointSlice(context.Background(), endpointSlice("any"))
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(requests), requests)
	}
	if requests[0].Namespace != ruleNamespace || requests[0].Name != "game" {
		t.Errorf("request = %s, want %s/game", requests[0].NamespacedName, ruleNamespace)
	}

	unlabelled := endpointSlice("orphan")
	unlabelled.Labels = nil
	if got := r.rulesForEndpointSlice(context.Background(), unlabelled); len(got) != 0 {
		t.Errorf("unlabelled slice mapped to %d rules", len(got))
	}
}

func TestReconcileDisablesAndReenablesRouterRule(t *testing.T) {
	reconciler, mockRouter := newRuleReconciler(t, loadBalancerService("web", "default", "http", 8080, "192.168.1.50"))
	rule := &v1alpha1.PortForwardRule{
		ObjectMeta: metav1.ObjectMeta{Name: "web-rule", Namespace: "default"},
		Spec: v1alpha1.PortForwardRuleSpec{
			ExternalPort:           intstr.FromInt32(8080),
			Protocol:               "tcp",
			Enabled:                true,
			ServiceRef:             &v1alpha1.ServiceReference{Name: "web", Port: "http"},
			SuspendWhenNoEndpoints: true,
		},
	}

	enabled := func() bool {
		t.Helper()
		rules := mockRouter.GetPortForwardRules()
		if len(rules) != 1 {
			t.Fatalf("got %d router rules, want 1: %+v", len(rules), rules)
		}
		return rules[0].Enabled
	}

	if err := reconciler.reconcilePortForwardRule(context.Background(), rule, true); err != nil {
		t.Fatalf("suspended reconcile: %v", err)
	}
	if enabled() {
		t.Error("router rule enabled while suspended")
	}

	if err := reconciler.reconcilePortForwardRule(context.Background(), rule, false); err != nil {
		t.Fatalf("resumed reconcile: %v", err)
	}
	if !enabled() {
		t.Error("router rule still disabled after resuming")
	}
}

func TestValidateRejectsSuspendWithoutServiceRef(t *testing.T) {
	ip := "192.168.1.50"
	port := intstr.FromInt32(8080)
	rule := &v1alpha1.PortForwardRule{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
		Spec: v1alpha1.PortForwardRuleSpec{
			ExternalPort:           intstr.FromInt32(8080),
			Protocol:               "tcp",
			DestinationIP:          &ip,
			DestinationPort:        &port,
			SuspendWhenNoEndpoints: true,
		},
	}
	if errs := rule.ValidateCreate(); len(errs) == 0 {
		t.Fatal("accepted suspendWhenNoEndpoints without serviceRef")
	}
}
