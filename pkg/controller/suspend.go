package controller

import (
	"context"
	"fmt"
	"time"

	"unifi-port-forward/pkg/api/v1alpha1"
	"unifi-port-forward/pkg/config"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// serviceRefIndex indexes PortForwardRules by the "namespace/name" of the
// Service they reference, so an EndpointSlice event can find its rules without
// listing every rule in the cluster.
const serviceRefIndex = "spec.serviceRef.namespacedName"

// serviceRefKey is the resolved "namespace/name" of a rule's Service, or "" for
// a rule without one. A serviceRef with no namespace means the rule's own.
func serviceRefKey(rule *v1alpha1.PortForwardRule) string {
	if rule.Spec.ServiceRef == nil {
		return ""
	}
	namespace := rule.Namespace
	if rule.Spec.ServiceRef.Namespace != nil && *rule.Spec.ServiceRef.Namespace != "" {
		namespace = *rule.Spec.ServiceRef.Namespace
	}
	return namespace + "/" + rule.Spec.ServiceRef.Name
}

// indexServiceRef is the IndexerFunc for serviceRefIndex.
func indexServiceRef(obj client.Object) []string {
	rule, ok := obj.(*v1alpha1.PortForwardRule)
	if !ok {
		return nil
	}
	if key := serviceRefKey(rule); key != "" {
		return []string{key}
	}
	return nil
}

// rulesForEndpointSlice maps an EndpointSlice event to the rules that reference
// its Service. Only rules that opted into suspension care about endpoints, so
// the rest are left to their normal requeue.
func (r *PortForwardRuleReconciler) rulesForEndpointSlice(ctx context.Context, obj client.Object) []reconcile.Request {
	serviceName := obj.GetLabels()[discoveryv1.LabelServiceName]
	if serviceName == "" {
		return nil
	}

	rules := &v1alpha1.PortForwardRuleList{}
	key := obj.GetNamespace() + "/" + serviceName
	if err := r.List(ctx, rules, client.MatchingFields{serviceRefIndex: key}); err != nil {
		ctrllog.FromContext(ctx).Error(err, "Failed to list rules for EndpointSlice", "service", key)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(rules.Items))
	for i := range rules.Items {
		if !rules.Items[i].Spec.SuspendWhenNoEndpoints {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: rules.Items[i].Namespace,
			Name:      rules.Items[i].Name,
		}})
	}
	return requests
}

// serviceHasEndpoints reports whether any EndpointSlice of the Service lists at
// least one endpoint. Readiness is deliberately ignored: a pod that is booting,
// updating or crash-looping still exists and should keep its port. Only "no
// pod at all" - which is what scaling to zero produces - counts as absent.
func (r *PortForwardRuleReconciler) serviceHasEndpoints(ctx context.Context, namespace, name string) (bool, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: name},
	); err != nil {
		return false, fmt.Errorf("listing EndpointSlices for %s/%s: %w", namespace, name, err)
	}
	for i := range slices.Items {
		if len(slices.Items[i].Endpoints) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// suspension is the outcome of evaluating a rule's endpoint state.
type suspension struct {
	// Suspended means the router rule should be disabled.
	Suspended bool
	// RecheckAfter is how long until a pending suspension takes effect, or zero
	// when nothing is pending.
	RecheckAfter time.Duration
	// Message explains a suspended or pending state for status and events.
	Message string
}

func (r *PortForwardRuleReconciler) suspendGrace() time.Duration {
	if r.Config != nil && r.Config.SuspendGrace > 0 {
		return r.Config.SuspendGrace
	}
	return config.DefaultSuspendGrace
}

// evaluateSuspension decides whether the rule's router entry should be disabled
// because its Service has no endpoints, and records when that started in
// rule.Status.NoEndpointsSince. The caller persists status.
func (r *PortForwardRuleReconciler) evaluateSuspension(ctx context.Context, rule *v1alpha1.PortForwardRule, now time.Time) (suspension, error) {
	if !rule.Spec.SuspendWhenNoEndpoints || rule.Spec.ServiceRef == nil {
		rule.Status.NoEndpointsSince = nil
		return suspension{}, nil
	}

	namespace := rule.Namespace
	if rule.Spec.ServiceRef.Namespace != nil && *rule.Spec.ServiceRef.Namespace != "" {
		namespace = *rule.Spec.ServiceRef.Namespace
	}

	hasEndpoints, err := r.serviceHasEndpoints(ctx, namespace, rule.Spec.ServiceRef.Name)
	if err != nil {
		return suspension{}, err
	}
	if hasEndpoints {
		rule.Status.NoEndpointsSince = nil
		return suspension{}, nil
	}

	if rule.Status.NoEndpointsSince == nil {
		rule.Status.NoEndpointsSince = &metav1.Time{Time: now}
	}

	grace := r.suspendGrace()
	elapsed := now.Sub(rule.Status.NoEndpointsSince.Time)
	if elapsed >= grace {
		return suspension{
			Suspended: true,
			Message: fmt.Sprintf("Service %s/%s has had no endpoints since %s",
				namespace, rule.Spec.ServiceRef.Name, rule.Status.NoEndpointsSince.UTC().Format(time.RFC3339)),
		}, nil
	}

	return suspension{
		RecheckAfter: grace - elapsed,
		Message: fmt.Sprintf("Service %s/%s has no endpoints; suspending in %s",
			namespace, rule.Spec.ServiceRef.Name, (grace - elapsed).Round(time.Second)),
	}, nil
}
