package utils

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"unifi-port-forward/pkg/ports"
	"unifi-port-forward/pkg/routers"
)

// PortMapping represents parsed annotation mapping
type PortMapping struct {
	PortName      string     // Service port name
	ExternalPorts ports.Spec // External port(s) (DstPort); empty means "use the service port"
}

// GetLBIP extracts the LoadBalancer IP from a service
func GetLBIP(service *v1.Service) string {
	if len(service.Status.LoadBalancer.Ingress) > 0 {
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				return ingress.IP
			}
		}
	}

	return ""
}

// parsePortMappingAnnotation parses port mapping annotation like "1234:http,8443:https"
//
// The external side of a mapping may be a single port or an inclusive range
// ("8000-8100:game"). Commas separate mappings, so a port *list* is written by
// pointing several mappings at the same service port name - "80:https,443:https"
// yields one rule forwarding both 80 and 443.
func parsePortMappingAnnotation(annotation string) ([]PortMapping, error) {
	if annotation == "" {
		return nil, nil
	}

	var mappings []PortMapping
	indexByName := make(map[string]int)

	for part := range strings.SplitSeq(annotation, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		mapping, err := parseSingleMapping(part)
		if err != nil {
			return nil, fmt.Errorf("invalid port mapping '%s': %w", part, err)
		}

		// Coalesce repeated port names into a single rule covering every port.
		if idx, seen := indexByName[mapping.PortName]; seen {
			merged, err := ports.Union(mappings[idx].ExternalPorts, mapping.ExternalPorts)
			if err != nil {
				return nil, fmt.Errorf("invalid port mapping '%s': %w", part, err)
			}
			mappings[idx].ExternalPorts = merged
			continue
		}

		indexByName[mapping.PortName] = len(mappings)
		mappings = append(mappings, mapping)
	}

	return mappings, nil
}

// parseSingleMapping parses individual port mapping like "1234:http", "8000-8100:game" or "https"
func parseSingleMapping(mapping string) (PortMapping, error) {
	parts := strings.Split(mapping, ":")

	switch len(parts) {
	case 1:
		// Default mapping: "http" -> use service port as external port
		return PortMapping{
			PortName: parts[0],
			// ExternalPorts stays empty and is filled from the service port later
		}, nil

	case 2:
		// Custom mapping: "1234:http" or "8000-8100:game" (externalPorts:serviceName)
		externalPorts, err := ports.Parse(parts[0])
		if err != nil {
			return PortMapping{}, fmt.Errorf("invalid external port '%s' in mapping '%s': %w. Valid format: 'externalPort:portname', 'externalPortRange:portname' or 'portname'. Example: '8080:http,8443:https,8000-8100:game'", parts[0], mapping, err)
		}

		return PortMapping{
			PortName:      parts[1],
			ExternalPorts: externalPorts,
		}, nil

	default:
		return PortMapping{}, fmt.Errorf("invalid mapping format: too many colons in '%s'. Valid format: 'externalPort:portname', 'externalPortRange:portname' or 'portname'. Example: '8080:http,8443:https,8000-8100:game'", mapping)
	}
}

// resolvePortSpecs derives the external and forward port specs for one mapped service port.
//
//   - a single external port forwards to the service port, as it always has
//   - a contiguous external range forwards to a same-sized range starting at the
//     service port, so "8000-8100:game" on a service port of 8000 preserves ports
//   - a discontinuous external list forwards every one of its ports to the
//     service port, so "80:https,443:https" on a service port of 443 collapses
func resolvePortSpecs(mapping PortMapping, servicePort int) (ports.Spec, ports.Spec, error) {
	external := mapping.ExternalPorts
	if external.IsEmpty() {
		external = ports.FromPort(servicePort)
	}

	if external.IsSinglePort() || !external.IsContiguous() {
		return external, ports.FromPort(servicePort), nil
	}

	forward, err := ports.ContiguousFrom(servicePort, external.Count())
	if err != nil {
		return ports.Spec{}, ports.Spec{}, fmt.Errorf("cannot forward external ports %s to service port %d: %w", external, servicePort, err)
	}
	return external, forward, nil
}

// validatePortMappings validates that all mapped port names exist in service and no conflicts
func validatePortMappings(service *v1.Service, mappings []PortMapping) error {
	// Check that all mapped port names exist in service
	servicePortNames := make(map[string]bool)
	for _, port := range service.Spec.Ports {
		servicePortNames[port.Name] = true
	}

	// Build available ports list for better error message
	var availablePorts []string
	for _, port := range service.Spec.Ports {
		availablePorts = append(availablePorts, fmt.Sprintf("%s(%d)", port.Name, port.Port))
	}

	for _, mapping := range mappings {
		if !servicePortNames[mapping.PortName] {
			return fmt.Errorf("port mapping references non-existent port '%s' in service %s/%s - available ports: %s. Valid format: 'externalPort:portname', 'externalPortRange:portname' or 'portname'. Example: '8080:http,8443:https,8000-8100:game'",
				mapping.PortName, service.Namespace, service.Name, strings.Join(availablePorts, ", "))
		}
	}

	// Check for overlapping external ports within this service
	type claim struct {
		portName string
		external ports.Spec
	}
	var claims []claim
	for _, port := range service.Spec.Ports {
		for _, mapping := range mappings {
			if mapping.PortName != port.Name {
				continue
			}

			external, _, err := resolvePortSpecs(mapping, int(port.Port))
			if err != nil {
				return err
			}

			for _, existing := range claims {
				if existing.external.Overlaps(external) {
					return fmt.Errorf("duplicate external port %s within service - '%s' overlaps with '%s' (%s)",
						external, mapping.PortName, existing.portName, existing.external)
				}
			}
			claims = append(claims, claim{portName: mapping.PortName, external: external})
		}
	}

	return nil
}

// GetPortNameByNumber returns the port name for a given port number from service spec
func GetPortNameByNumber(service *v1.Service, portNumber int) string {
	for _, port := range service.Spec.Ports {
		if int(port.Port) == portNumber {
			if port.Name != "" {
				return port.Name
			}
			// Fallback to port if no name is set
			return string(port.Port)
		}
	}
	return fmt.Sprintf("%d", portNumber)
}

// GetServicePortByName returns the port config for a given port name from service spec
func GetServicePortByName(service *v1.Service, portName string) *v1.ServicePort {
	for _, port := range service.Spec.Ports {
		if port.Name == portName {
			return &port
		}
	}
	return nil
}

// GetPortConfigs creates multiple PortConfigs from a service (supports multiple ports)
func GetPortConfigs(service *v1.Service, lbIP, annotationKey string) ([]routers.PortConfig, error) {
	serviceKey := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	// Parse annotation
	annotation := service.Annotations[annotationKey]
	if annotation == "" {
		return nil, fmt.Errorf("no port annotation found")
	}

	mappings, err := parsePortMappingAnnotation(annotation)
	if err != nil {
		return nil, fmt.Errorf("failed to parse port mapping: %w", err)
	}

	// Validate mappings against service definition
	if err := validatePortMappings(service, mappings); err != nil {
		return nil, err
	}

	var configs []routers.PortConfig

	// Create PortConfig for each service port
	for _, servicePort := range service.Spec.Ports {
		// Find matching annotation mapping
		var matched PortMapping
		var foundMapping bool

		for _, mapping := range mappings {
			if mapping.PortName == servicePort.Name {
				matched = mapping
				foundMapping = true
				break
			}
		}

		// Skip ports not mentioned in annotation
		if !foundMapping {
			continue
		}

		externalPorts, forwardPorts, err := resolvePortSpecs(matched, int(servicePort.Port))
		if err != nil {
			return nil, err
		}

		// Check for port conflicts with other services
		if err := CheckPortConflict(externalPorts, serviceKey); err != nil {
			return nil, err
		}

		// Mark these ports as used by this service
		markPortsUsed(externalPorts, serviceKey)

		protocol := strings.ToLower(string(servicePort.Protocol))

		configs = append(configs, routers.PortConfig{
			Name:      fmt.Sprintf("%s/%s:%s", service.Namespace, service.Name, servicePort.Name),
			DstPort:   externalPorts, // External port(s) from annotation
			FwdPort:   forwardPorts,  // Internal service port(s)
			Enabled:   true,
			Interface: "wan",
			DstIP:     lbIP,
			SrcIP:     "any",
			Protocol:  protocol,
		})
	}

	return configs, nil
}
