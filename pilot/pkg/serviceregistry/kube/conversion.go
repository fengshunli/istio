// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/gateway-api/apis/v1beta1"

	"istio.io/api/annotation"
	"istio.io/api/label"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/kube"
	"istio.io/istio/pkg/config/visibility"
	"istio.io/istio/pkg/spiffe"
)

func convertPort(port corev1.ServicePort) *model.Port {
	return &model.Port{
		Name:     port.Name,
		Port:     int(port.Port),
		Protocol: kube.ConvertProtocol(port.Port, port.Name, port.Protocol, port.AppProtocol),
	}
}

func ConvertService(svc corev1.Service, domainSuffix string, clusterID cluster.ID) *model.Service {
	addrs := []string{constants.UnspecifiedIP}
	resolution := model.ClientSideLB
	externalName := ""
	nodeLocal := false

	if svc.Spec.Type == corev1.ServiceTypeExternalName && svc.Spec.ExternalName != "" {
		resolution = model.DNSLB
		externalName = svc.Spec.ExternalName
	}
	if svc.Spec.InternalTrafficPolicy != nil && *svc.Spec.InternalTrafficPolicy == corev1.ServiceInternalTrafficPolicyLocal {
		nodeLocal = true
	}

	if svc.Spec.ClusterIP == corev1.ClusterIPNone { // headless services should not be load balanced
		resolution = model.Passthrough
	} else if svc.Spec.ClusterIP != "" {
		addrs[0] = svc.Spec.ClusterIP
		if len(svc.Spec.ClusterIPs) > 1 {
			addrs = svc.Spec.ClusterIPs
		}
	}

	ports := make([]*model.Port, 0, len(svc.Spec.Ports))
	for _, port := range svc.Spec.Ports {
		ports = append(ports, convertPort(port))
	}

	var exportTo map[visibility.Instance]bool
	serviceaccounts := make([]string, 0)
	if svc.Annotations[annotation.AlphaCanonicalServiceAccounts.Name] != "" {
		serviceaccounts = append(serviceaccounts, strings.Split(svc.Annotations[annotation.AlphaCanonicalServiceAccounts.Name], ",")...)
	}
	if svc.Annotations[annotation.AlphaKubernetesServiceAccounts.Name] != "" {
		for _, ksa := range strings.Split(svc.Annotations[annotation.AlphaKubernetesServiceAccounts.Name], ",") {
			serviceaccounts = append(serviceaccounts, kubeToIstioServiceAccount(ksa, svc.Namespace))
		}
	}
	if svc.Annotations[annotation.NetworkingExportTo.Name] != "" {
		namespaces := strings.Split(svc.Annotations[annotation.NetworkingExportTo.Name], ",")
		exportTo = make(map[visibility.Instance]bool, len(namespaces))
		for _, ns := range namespaces {
			exportTo[visibility.Instance(ns)] = true
		}
	}

	istioService := &model.Service{
		Hostname: ServiceHostname(svc.Name, svc.Namespace, domainSuffix),
		ClusterVIPs: model.AddressMap{
			Addresses: map[cluster.ID][]string{
				clusterID: addrs,
			},
		},
		Ports:           ports,
		DefaultAddress:  addrs[0],
		ServiceAccounts: serviceaccounts,
		MeshExternal:    len(externalName) > 0,
		Resolution:      resolution,
		CreationTime:    svc.CreationTimestamp.Time,
		ResourceVersion: svc.ResourceVersion,
		Attributes: model.ServiceAttributes{
			ServiceRegistry: provider.Kubernetes,
			Name:            svc.Name,
			Namespace:       svc.Namespace,
			Labels:          svc.Labels,
			ExportTo:        exportTo,
			LabelSelectors:  svc.Spec.Selector,
		},
	}

	switch svc.Spec.Type {
	case corev1.ServiceTypeNodePort:
		if _, ok := svc.Annotations[annotation.TrafficNodeSelector.Name]; !ok {
			// only do this for istio ingress-gateway services
			break
		}
		// store the service port to node port mappings
		portMap := make(map[uint32]uint32)
		for _, p := range svc.Spec.Ports {
			portMap[uint32(p.Port)] = uint32(p.NodePort)
		}
		istioService.Attributes.ClusterExternalPorts = map[cluster.ID]map[uint32]uint32{clusterID: portMap}
		// address mappings will be done elsewhere
	case corev1.ServiceTypeLoadBalancer:
		if len(svc.Status.LoadBalancer.Ingress) > 0 {
			var lbAddrs []string
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if len(ingress.IP) > 0 {
					lbAddrs = append(lbAddrs, ingress.IP)
				} else if len(ingress.Hostname) > 0 {
					// DO NOT resolve the DNS here. In environments like AWS, the ELB hostname
					// does not have a repeatable DNS address and IPs resolved at an earlier point
					// in time may not work. So, when we get just hostnames instead of IPs, we need
					// to smartly switch from EDS to strict_dns rather than doing the naive thing of
					// resolving the DNS name and hoping the resolution is one-time task.
					lbAddrs = append(lbAddrs, ingress.Hostname)
				}
			}
			if len(lbAddrs) > 0 {
				if istioService.Attributes.ClusterExternalAddresses == nil {
					istioService.Attributes.ClusterExternalAddresses = &model.AddressMap{}
				}
				istioService.Attributes.ClusterExternalAddresses.SetAddressesFor(clusterID, lbAddrs)
			}
		}
	}

	istioService.Attributes.Type = string(svc.Spec.Type)
	istioService.Attributes.ExternalName = externalName
	istioService.Attributes.NodeLocal = nodeLocal
	if len(svc.Spec.ExternalIPs) > 0 {
		if istioService.Attributes.ClusterExternalAddresses == nil {
			istioService.Attributes.ClusterExternalAddresses = &model.AddressMap{}
		}
		istioService.Attributes.ClusterExternalAddresses.AddAddressesFor(clusterID, svc.Spec.ExternalIPs)
	}

	return istioService
}

func ExternalNameServiceInstances(k8sSvc *corev1.Service, svc *model.Service) []*model.ServiceInstance {
	if k8sSvc == nil || k8sSvc.Spec.Type != corev1.ServiceTypeExternalName || k8sSvc.Spec.ExternalName == "" {
		return nil
	}
	out := make([]*model.ServiceInstance, 0, len(svc.Ports))

	discoverabilityPolicy := model.AlwaysDiscoverable
	if features.EnableMCSServiceDiscovery {
		// MCS spec does not allow export of external name services.
		// See https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/1645-multi-cluster-services-api#exporting-services.
		discoverabilityPolicy = model.DiscoverableFromSameCluster
	}
	for _, portEntry := range svc.Ports {
		out = append(out, &model.ServiceInstance{
			Service:     svc,
			ServicePort: portEntry,
			Endpoint: &model.IstioEndpoint{
				Address:               k8sSvc.Spec.ExternalName,
				EndpointPort:          uint32(portEntry.Port),
				ServicePortName:       portEntry.Name,
				Labels:                k8sSvc.Labels,
				DiscoverabilityPolicy: discoverabilityPolicy,
			},
		})
	}
	return out
}

// ServiceHostname produces FQDN for a k8s service
func ServiceHostname(name, namespace, domainSuffix string) host.Name {
	return host.Name(name + "." + namespace + "." + "svc" + "." + domainSuffix) // Format: "%s.%s.svc.%s"
}

// ServiceHostnameForKR calls ServiceHostname with the name and namespace of the given kubernetes resource.
func ServiceHostnameForKR(obj metav1.Object, domainSuffix string) host.Name {
	return ServiceHostname(obj.GetName(), obj.GetNamespace(), domainSuffix)
}

// kubeToIstioServiceAccount converts a K8s service account to an Istio service account
func kubeToIstioServiceAccount(saname string, ns string) string {
	return spiffe.MustGenSpiffeURI(ns, saname)
}

// SecureNamingSAN creates the secure naming used for SAN verification from pod metadata
func SecureNamingSAN(pod *corev1.Pod) string {
	return spiffe.MustGenSpiffeURI(pod.Namespace, pod.Spec.ServiceAccountName)
}

// PodTLSMode returns the tls mode associated with the pod if pod has been injected with sidecar
func PodTLSMode(pod *corev1.Pod) string {
	if pod == nil {
		return model.DisabledTLSModeLabel
	}
	return model.GetTLSModeFromEndpointLabels(pod.Labels)
}

// IsAutoPassthrough determines if a listener should use auto passthrough mode. This is used for
// multi-network. In the Istio API, this is an explicit tls.Mode. However, this mode is not part of
// the gateway-api, and leaks implementation details. We already have an API to declare a Gateway as
// a multi-network gateway, so we will use this as a signal.
// A user who wishes to expose multi-network connectivity should create a listener named "tls-passthrough"
// with TLS.Mode Passthrough.
// For some backwards compatibility, we assume any listener with TLS specified and a port matching
// 15443 (or the label-override for gateway port) is auto-passtrough as well.
func IsAutoPassthrough(gwLabels map[string]string, l v1beta1.Listener) bool {
	if l.TLS == nil {
		return false
	}
	if hasListenerMode(l, constants.ListenerModeAutoPassthrough) {
		return true
	}
	_, networkSet := gwLabels[label.TopologyNetwork.Name]
	if !networkSet {
		return false
	}
	expectedPort := "15443"
	if port, f := gwLabels[label.NetworkingGatewayPort.Name]; f {
		expectedPort = port
	}
	return fmt.Sprint(l.Port) == expectedPort
}

func hasListenerMode(l v1beta1.Listener, mode string) bool {
	// TODO if we add a hybrid mode for detecting HBONE/passthrough, also check that here
	return l.TLS != nil && l.TLS.Options != nil && string(l.TLS.Options[constants.ListenerModeOption]) == mode
}
