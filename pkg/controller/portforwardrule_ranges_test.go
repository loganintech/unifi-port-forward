package controller

import (
	"context"
	"testing"

	"github.com/filipowm/go-unifi/unifi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"

	"unifi-port-forward/pkg/api/v1alpha1"
	"unifi-port-forward/pkg/config"
	"unifi-port-forward/pkg/ports"
	"unifi-port-forward/pkg/routers"
	"unifi-port-forward/testutils"
)

func newRuleReconciler(t *testing.T, services ...*corev1.Service) (*PortForwardRuleReconciler, *testutils.MockRouter) {
	t.Helper()

	mockRouter := testutils.NewMockRouter()
	mockRouter.ClearAllPortForwards()
	mockRouter.ResetOperationCounts()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add core v1 to scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add v1alpha1 to scheme: %v", err)
	}

	fakeClient := testutils.NewFakeKubernetesClient(t, scheme)
	for _, service := range services {
		if err := fakeClient.Create(context.Background(), service); err != nil {
			t.Fatalf("Failed to seed service %s/%s: %v", service.Namespace, service.Name, err)
		}
	}

	return &PortForwardRuleReconciler{
		Client:   fakeClient,
		Router:   mockRouter,
		Scheme:   scheme,
		Config:   &config.Config{Debug: true},
		Recorder: &record.FakeRecorder{},
	}, mockRouter
}

// loadBalancerService builds a LoadBalancer Service with a single named port.
func loadBalancerService(name, namespace, portName string, port int32, ip string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: portName, Port: port, Protocol: corev1.ProtocolTCP}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: ip}},
			},
		},
	}
}

func TestGetServiceDestinationDerivesInternalPorts(t *testing.T) {
	tests := []struct {
		name          string
		externalPorts string
		servicePort   int32
		wantPorts     string
	}{
		{
			name:          "single external port maps to the service port",
			externalPorts: "8080",
			servicePort:   1234,
			wantPorts:     "1234",
		},
		{
			name:          "range maps to a same-sized range at the service port",
			externalPorts: "8000-8002",
			servicePort:   30000,
			wantPorts:     "30000-30002",
		},
		{
			name:          "list collapses onto the service port",
			externalPorts: "80,443",
			servicePort:   8443,
			wantPorts:     "8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := loadBalancerService("web", "default", "http", tt.servicePort, "192.168.1.50")
			reconciler, _ := newRuleReconciler(t, service)

			rule := &v1alpha1.PortForwardRule{
				ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: "default"},
				Spec: v1alpha1.PortForwardRuleSpec{
					ExternalPort: intstr.FromString(tt.externalPorts),
					Protocol:     "tcp",
					ServiceRef:   &v1alpha1.ServiceReference{Name: "web", Port: "http"},
				},
			}

			destIP, destPorts, err := reconciler.getServiceDestination(
				context.Background(), rule, ports.MustParse(tt.externalPorts))
			if err != nil {
				t.Fatalf("getServiceDestination returned error: %v", err)
			}

			if destIP != "192.168.1.50" {
				t.Errorf("destination IP = %q, want 192.168.1.50", destIP)
			}
			if got := destPorts.String(); got != tt.wantPorts {
				t.Errorf("destination ports = %q, want %q", got, tt.wantPorts)
			}
		})
	}
}

func TestGetServiceDestinationRejectsRangePastMaxPort(t *testing.T) {
	service := loadBalancerService("web", "default", "http", 65530, "192.168.1.50")
	reconciler, _ := newRuleReconciler(t, service)

	rule := &v1alpha1.PortForwardRule{
		ObjectMeta: metav1.ObjectMeta{Name: "rule", Namespace: "default"},
		Spec: v1alpha1.PortForwardRuleSpec{
			ExternalPort: intstr.FromString("8000-8009"),
			Protocol:     "tcp",
			ServiceRef:   &v1alpha1.ServiceReference{Name: "web", Port: "http"},
		},
	}

	if _, _, err := reconciler.getServiceDestination(
		context.Background(), rule, ports.MustParse("8000-8009")); err == nil {
		t.Error("expected an error when the derived internal range runs past the maximum port")
	}
}

func TestReconcilePortForwardRuleCreatesRangeRule(t *testing.T) {
	service := loadBalancerService("game", "default", "udp", 27015, "192.168.1.60")
	reconciler, mockRouter := newRuleReconciler(t, service)

	rule := &v1alpha1.PortForwardRule{
		ObjectMeta: metav1.ObjectMeta{Name: "game-rule", Namespace: "default"},
		Spec: v1alpha1.PortForwardRuleSpec{
			ExternalPort: intstr.FromString("27015-27020"),
			Protocol:     "udp",
			Interface:    "wan",
			Enabled:      true,
			ServiceRef:   &v1alpha1.ServiceReference{Name: "game", Port: "udp"},
		},
	}

	if err := reconciler.reconcilePortForwardRule(context.Background(), rule); err != nil {
		t.Fatalf("reconcilePortForwardRule returned error: %v", err)
	}

	created := mockRouter.GetPortForwardRules()
	if len(created) != 1 {
		t.Fatalf("expected 1 router rule, got %d: %+v", len(created), created)
	}

	if created[0].DstPort != "27015-27020" {
		t.Errorf("router DstPort = %q, want 27015-27020", created[0].DstPort)
	}
	if created[0].FwdPort != "27015-27020" {
		t.Errorf("router FwdPort = %q, want 27015-27020", created[0].FwdPort)
	}
	if created[0].Name != "default/game-rule:27015-27020" {
		t.Errorf("router rule name = %q, want default/game-rule:27015-27020", created[0].Name)
	}

	// The print column reads status.externalPorts, so it must always be a string
	// spec regardless of how the user wrote spec.externalPort.
	if rule.Status.ExternalPorts != "27015-27020" {
		t.Errorf("status ExternalPorts = %q, want 27015-27020", rule.Status.ExternalPorts)
	}

	if rule.Status.ServiceStatus == nil {
		t.Fatal("expected service status to be populated")
	}
	if rule.Status.ServiceStatus.ServicePorts != "27015-27020" {
		t.Errorf("status ServicePorts = %q, want 27015-27020", rule.Status.ServiceStatus.ServicePorts)
	}
	if rule.Status.ServiceStatus.ServicePort != 27015 {
		t.Errorf("status ServicePort = %d, want 27015", rule.Status.ServiceStatus.ServicePort)
	}
}

// TestRulePortKeyNormalizesRouterFormatting guards the drift comparison: a rule
// the router reports as "80,81" is the same rule we asked for as "80-81", and
// must not read as drift.
func TestRulePortKeyNormalizesRouterFormatting(t *testing.T) {
	routerRule := &unifi.PortForward{DstPort: "80,81", FwdPort: "8080,8081", Proto: "tcp"}

	desired := routers.PortConfig{
		DstPort:  ports.MustParse("80-81"),
		FwdPort:  ports.MustParse("8080-8081"),
		Protocol: "tcp",
	}

	got := rulePortKey(routerRule)
	want := portKey(desired.DstPort, desired.FwdPort, desired.Protocol)
	if got != want {
		t.Errorf("rulePortKey = %q, want %q", got, want)
	}

	if mismatch := determineMismatchType(routerRule, desired, &ChangeContext{}); mismatch == "port" || mismatch == "fwdport" {
		t.Errorf("equivalent port specs should not report a port mismatch, got %q", mismatch)
	}
}
