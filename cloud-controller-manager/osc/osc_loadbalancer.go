//go:build !providerless
// +build !providerless

/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package osc

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/outscale/osc-sdk-go/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// ProxyProtocolPolicyName is the tag named used for the proxy protocol
	// policy
	ProxyProtocolPolicyName = "k8s-proxyprotocol-enabled"

	// SSLNegotiationPolicyNameFormat is a format string used for the SSL
	// negotiation policy tag name
	SSLNegotiationPolicyNameFormat = "k8s-SSLNegotiationPolicy-%s"

	lbAttrLoadBalancingCrossZoneEnabled = "load_balancing.cross_zone.enabled"
	lbAttrAccessLogsS3Enabled           = "access_logs.s3.enabled"
	lbAttrAccessLogsS3Bucket            = "access_logs.s3.bucket"
	lbAttrAccessLogsS3Prefix            = "access_logs.s3.prefix"
)

var (
	// Defaults for ELB Healthcheck
	defaultHCHealthyThreshold   = int64(2)
	defaultHCUnhealthyThreshold = int64(6)
	defaultHCTimeout            = int64(5)
	defaultHCInterval           = int64(10)
)

// getLoadBalancerAdditionalTags converts the comma separated list of key-value
// pairs in the ServiceAnnotationLoadBalancerAdditionalTags annotation and returns
// it as a map.
func getLoadBalancerAdditionalTags(annotations map[string]string) map[string]string {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("getLoadBalancerAdditionalTags(%v)", annotations)
	additionalTags := make(map[string]string)
	if additionalTagsList, ok := annotations[ServiceAnnotationLoadBalancerAdditionalTags]; ok {
		additionalTagsList = strings.TrimSpace(additionalTagsList)

		// Break up list of "Key1=Val,Key2=Val2"
		tagList := strings.Split(additionalTagsList, ",")

		// Break up "Key=Val"
		for _, tagSet := range tagList {
			tag := strings.Split(strings.TrimSpace(tagSet), "=")

			// Accept "Key=val" or "Key=" or just "Key"
			if len(tag) >= 2 && len(tag[0]) != 0 {
				// There is a key and a value, so save it
				additionalTags[tag[0]] = tag[1]
			} else if len(tag) == 1 && len(tag[0]) != 0 {
				// Just "Key"
				additionalTags[tag[0]] = ""
			}
		}
	}
	return additionalTags
}

func (c *Cloud) ensureLoadBalancer(namespacedName types.NamespacedName, loadBalancerName string,
	listeners []*elb.Listener, subnetIDs []string, securityGroupIDs []string, internalELB,
	proxyProtocol bool, loadBalancerAttributes *elb.LoadBalancerAttributes,
	annotations map[string]string) (*elb.LoadBalancerDescription, error) {

	debugPrintCallerFunctionName()
	klog.V(5).Infof("ensureLoadBalancer(%v,%v,%v,%v,%v,%v,%v,%v,%v,)",
		namespacedName, loadBalancerName, listeners, subnetIDs, securityGroupIDs,
		internalELB, proxyProtocol, loadBalancerAttributes, annotations)

	loadBalancer, err := c.describeLoadBalancer(loadBalancerName)
	if err != nil {
		return nil, err
	}

	dirty := false

	if loadBalancer == nil {
		createRequest := &elb.CreateLoadBalancerInput{}
		createRequest.LoadBalancerName = aws.String(loadBalancerName)

		createRequest.Listeners = listeners

		if internalELB {
			createRequest.Scheme = aws.String("internal")
		}

		// We are supposed to specify one subnet per AZ.
		// TODO: What happens if we have more than one subnet per AZ?
		if subnetIDs == nil {
			createRequest.Subnets = nil

			createRequest.AvailabilityZones = append(createRequest.AvailabilityZones, aws.String(c.selfAWSInstance.availabilityZone))
		} else {
			createRequest.Subnets = aws.StringSlice(subnetIDs)
		}

		if securityGroupIDs == nil || subnetIDs == nil {
			createRequest.SecurityGroups = nil
		} else {
			createRequest.SecurityGroups = aws.StringSlice(securityGroupIDs)
		}

		// Get additional tags set by the user
		tags := getLoadBalancerAdditionalTags(annotations)

		// Add default tags
		tags[TagNameKubernetesService] = namespacedName.String()
		tags = c.tagging.buildTags(ResourceLifecycleOwned, tags)

		for k, v := range tags {
			createRequest.Tags = append(createRequest.Tags, &elb.Tag{
				Key: aws.String(k), Value: aws.String(v),
			})
		}

		klog.Infof("Creating load balancer for %v with name: %s", namespacedName, loadBalancerName)
		klog.Infof("c.elb.CreateLoadBalancer(createRequest): %v", createRequest)

		_, err := c.loadBalancer.CreateLoadBalancer(createRequest)
		if err != nil {
			return nil, err
		}

		if proxyProtocol {
			err = c.createProxyProtocolPolicy(loadBalancerName, false)
			if err != nil {
				return nil, err
			}

			for _, listener := range listeners {
				klog.V(2).Infof("Adjusting AWS loadbalancer proxy protocol on node port %d. Setting to true", *listener.InstancePort)
				err := c.setBackendPolicies(loadBalancerName, *listener.InstancePort, []*string{aws.String(ProxyProtocolPolicyName)})
				if err != nil {
					return nil, err
				}
			}
		}

		dirty = true
	} else {
		// TODO: Sync internal vs non-internal
		{
			// Sync subnets
			expected := sets.NewString(subnetIDs...)
			actual := stringSetFromPointers(loadBalancer.Subnets)
			additions := expected.Difference(actual)
			removals := actual.Difference(expected)
			klog.Warningf("AttachLoadBalancerToSubnets/DetachLoadBalancerFromSubnets loadBalancer: %v / expected: %v / actual %v / additions %v / removals %v",
				loadBalancer, expected, actual, additions, removals)
			if removals.Len() != 0 {
				klog.Warningf("DetachLoadBalancerFromSubnets not supported loadBalancer: %v / expected: %v / actual %v / additions %v / removals %v",
					loadBalancer, expected, actual, additions, removals)
				dirty = true
			}
			if additions.Len() != 0 {
				klog.Warningf("AttachLoadBalancerToSubnets not supported loadBalancer: %v / expected: %v / actual %v / additions %v / removals %v",
					loadBalancer, expected, actual, additions, removals)
				dirty = true
			}
		}
		{
			// Sync security groups
			expected := sets.NewString(securityGroupIDs...)
			actual := stringSetFromPointers(loadBalancer.SecurityGroups)
			if len(subnetIDs) == 0 || c.vpcID == "" {
				actual = sets.NewString([]string{DefaultSrcSgName}...)
			}

			klog.Infof("ApplySecurityGroupsToLoadBalancer: loadBalancer: %v expected: %v / actual %v",
				loadBalancer, expected, actual)
			if !expected.Equal(actual) {
				klog.Warningf("ApplySecurityGroupsToLoadBalancer not supported loadBalancer: %v expected: %v / actual %v",
					loadBalancer, expected, actual)
			}
		}

		{
			additions, removals, removalsInstancePorts := syncElbListeners(loadBalancerName, listeners, loadBalancer.ListenerDescriptions)
			if len(removals) != 0 {
				request := &elb.DeleteLoadBalancerListenersInput{}
				request.LoadBalancerName = aws.String(loadBalancerName)
				request.LoadBalancerPorts = removals

				if proxyProtocol {
					for _, backendListener := range loadBalancer.BackendServerDescriptions {
						for _, instancePort := range removalsInstancePorts {
							if aws.Int64Value(backendListener.InstancePort) == aws.Int64Value(instancePort) {
								klog.V(2).Infof("Removing backend policies before removing Listener to prevent update error")
								err := c.setBackendPolicies(loadBalancerName, aws.Int64Value(instancePort), []*string{})
								if err != nil {
									return nil, err
								}
								break
							}
						}
					}
				}
				klog.V(2).Info("Deleting removed load balancer listeners")
				if _, err := c.loadBalancer.DeleteLoadBalancerListeners(request); err != nil {
					return nil, fmt.Errorf("error deleting OSC loadbalancer listeners: %q", err)
				}
				dirty = true
			}

			if len(additions) != 0 {
				request := &elb.CreateLoadBalancerListenersInput{}
				request.LoadBalancerName = aws.String(loadBalancerName)
				request.Listeners = additions
				klog.V(2).Info("Creating added load balancer listeners")
				if _, err := c.loadBalancer.CreateLoadBalancerListeners(request); err != nil {
					return nil, fmt.Errorf("error creating OSC loadbalancer listeners: %q", err)
				}
				dirty = true
			}
		}

		{
			// Sync proxy protocol state for new and existing listeners
			proxyPolicies := make([]*string, 0)
			if proxyProtocol {
				// Ensure the backend policy exists
				err := c.createProxyProtocolPolicy(loadBalancerName, true)
				if err != nil {
					return nil, err
				}
				proxyPolicies = append(proxyPolicies, aws.String(ProxyProtocolPolicyName))
			}

			foundBackends := make(map[int64]bool)
			proxyProtocolBackends := make(map[int64]bool)
			for _, backendListener := range loadBalancer.BackendServerDescriptions {
				foundBackends[*backendListener.InstancePort] = false
				proxyProtocolBackends[*backendListener.InstancePort] = proxyProtocolEnabled(backendListener)
			}

			for _, listener := range listeners {
				setPolicy := false
				instancePort := *listener.InstancePort

				if currentState, ok := proxyProtocolBackends[instancePort]; !ok {
					// This is a new ELB backend so we only need to worry about
					// potentially adding a policy and not removing an
					// existing one
					setPolicy = proxyProtocol
				} else {
					foundBackends[instancePort] = true
					// This is an existing ELB backend so we need to determine
					// if the state changed
					setPolicy = (currentState != proxyProtocol)
				}

				if setPolicy {
					klog.V(2).Infof("Adjusting AWS loadbalancer proxy protocol on node port %d. Setting to %t", instancePort, proxyProtocol)
					err := c.setBackendPolicies(loadBalancerName, instancePort, proxyPolicies)
					if err != nil {
						return nil, err
					}
					dirty = true
				}
			}
		}

		{
			// Add additional tags
			klog.V(2).Infof("Creating additional load balancer tags for %s", loadBalancerName)
			tags := getLoadBalancerAdditionalTags(annotations)
			if len(tags) > 0 {
				err := c.addLoadBalancerTags(loadBalancerName, tags)
				if err != nil {
					return nil, fmt.Errorf("unable to create additional load balancer tags: %v", err)
				}
			}
		}
	}

	// Whether the ELB was new or existing, sync attributes regardless. This accounts for things
	// that cannot be specified at the time of creation and can only be modified after the fact,
	// e.g. idle connection timeout.
	{
		describeAttributesRequest := &elb.DescribeLoadBalancerAttributesInput{}
		describeAttributesRequest.LoadBalancerName = aws.String(loadBalancerName)
		describeAttributesOutput, err := c.loadBalancer.DescribeLoadBalancerAttributes(describeAttributesRequest)
		if err != nil {
			klog.Warning("Unable to retrieve load balancer attributes during attribute sync")
			return nil, err
		}

		foundAttributes := &describeAttributesOutput.LoadBalancerAttributes

		// Update attributes if they're dirty
		if !reflect.DeepEqual(loadBalancerAttributes, foundAttributes) {
			modifyAttributesRequest := &elb.ModifyLoadBalancerAttributesInput{}
			modifyAttributesRequest.LoadBalancerName = aws.String(loadBalancerName)
			modifyAttributesRequest.LoadBalancerAttributes = loadBalancerAttributes
			klog.V(2).Infof("Updating load-balancer attributes for %q with attributes (%v)",
				loadBalancerName, loadBalancerAttributes)
			_, err = c.loadBalancer.ModifyLoadBalancerAttributes(modifyAttributesRequest)
			if err != nil {
				return nil, fmt.Errorf("Unable to update load balancer attributes during attribute sync: %q", err)
			}
			dirty = true
		}
	}

	if dirty {
		loadBalancer, err = c.describeLoadBalancer(loadBalancerName)
		if err != nil {
			klog.Warning("Unable to retrieve load balancer after creation/update")
			return nil, err
		}
	}

	return loadBalancer, nil
}

// syncElbListeners computes a plan to reconcile the desired vs actual state of the listeners on an ELB
// NOTE: there exists an O(nlgn) implementation for this function. However, as the default limit of
//       listeners per elb is 100, this implementation is reduced from O(m*n) => O(n).
func syncElbListeners(loadBalancerName string, listeners []*elb.Listener, listenerDescriptions []*elb.ListenerDescription) ([]*elb.Listener, []*int64, []*int64) {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("syncElbListeners(%v,%v,%v)", loadBalancerName, listeners, listenerDescriptions)
	foundSet := make(map[int]bool)
	removals := []*int64{}
	removalsInstancePorts := []*int64{}
	additions := []*elb.Listener{}

	for _, listenerDescription := range listenerDescriptions {
		actual := listenerDescription.Listener
		if actual == nil {
			klog.Warning("Ignoring empty listener in AWS loadbalancer: ", loadBalancerName)
			continue
		}

		found := false
		for i, expected := range listeners {
			if expected == nil {
				klog.Warning("Ignoring empty desired listener for loadbalancer: ", loadBalancerName)
				continue
			}
			if elbListenersAreEqual(actual, expected) {
				// The current listener on the actual
				// elb is in the set of desired listeners.
				foundSet[i] = true
				found = true
				break
			}
		}
		if !found {
			removals = append(removals, actual.LoadBalancerPort)
			removalsInstancePorts = append(removalsInstancePorts, actual.InstancePort)
		}
	}

	for i := range listeners {
		if !foundSet[i] {
			additions = append(additions, listeners[i])
		}
	}

	return additions, removals, removalsInstancePorts
}

func elbListenersAreEqual(actual, expected *elb.Listener) bool {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("elbListenersAreEqual(%v,%v)", actual, expected)
	if !elbProtocolsAreEqual(actual.Protocol, expected.Protocol) {
		return false
	}
	if !elbProtocolsAreEqual(actual.InstanceProtocol, expected.InstanceProtocol) {
		return false
	}
	if aws.Int64Value(actual.InstancePort) != aws.Int64Value(expected.InstancePort) {
		return false
	}
	if aws.Int64Value(actual.LoadBalancerPort) != aws.Int64Value(expected.LoadBalancerPort) {
		return false
	}
	if !awsArnEquals(actual.SSLCertificateId, expected.SSLCertificateId) {
		return false
	}
	return true
}

// elbProtocolsAreEqual checks if two ELB protocol strings are considered the same
// Comparison is case insensitive
func elbProtocolsAreEqual(l, r *string) bool {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("elbProtocolsAreEqual(%v,%v)", l, r)
	if l == nil || r == nil {
		return l == r
	}
	return strings.EqualFold(aws.StringValue(l), aws.StringValue(r))
}

// awsArnEquals checks if two ARN strings are considered the same
// Comparison is case insensitive
func awsArnEquals(l, r *string) bool {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("awsArnEquals(%v,%v)", l, r)
	if l == nil || r == nil {
		return l == r
	}
	return strings.EqualFold(aws.StringValue(l), aws.StringValue(r))
}

// getExpectedHealthCheck returns an elb.Healthcheck for the provided target
// and using either sensible defaults or overrides via Service annotations
func (c *Cloud) getExpectedHealthCheck(target string, annotations map[string]string) (*elb.HealthCheck, error) {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("getExpectedHealthCheck(%v,%v)", target, annotations)
	healthcheck := &elb.HealthCheck{Target: &target}
	getOrDefault := func(annotation string, defaultValue int64) (*int64, error) {
		i64 := defaultValue
		var err error
		if s, ok := annotations[annotation]; ok {
			i64, err = strconv.ParseInt(s, 10, 0)
			if err != nil {
				return nil, fmt.Errorf("failed parsing health check annotation value: %v", err)
			}
		}
		return &i64, nil
	}
	var err error
	healthcheck.HealthyThreshold, err = getOrDefault(ServiceAnnotationLoadBalancerHCHealthyThreshold, defaultHCHealthyThreshold)
	if err != nil {
		return nil, err
	}
	healthcheck.UnhealthyThreshold, err = getOrDefault(ServiceAnnotationLoadBalancerHCUnhealthyThreshold, defaultHCUnhealthyThreshold)
	if err != nil {
		return nil, err
	}
	healthcheck.Timeout, err = getOrDefault(ServiceAnnotationLoadBalancerHCTimeout, defaultHCTimeout)
	if err != nil {
		return nil, err
	}
	healthcheck.Interval, err = getOrDefault(ServiceAnnotationLoadBalancerHCInterval, defaultHCInterval)
	if err != nil {
		return nil, err
	}
	if err = healthcheck.Validate(); err != nil {
		return nil, fmt.Errorf("some of the load balancer health check parameters are invalid: %v", err)
	}
	return healthcheck, nil
}

// Makes sure that the health check for an ELB matches the configured health check node port
func (c *Cloud) ensureLoadBalancerHealthCheck(loadBalancer *elb.LoadBalancerDescription,
	protocol string, port int32, path string, annotations map[string]string) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("ensureLoadBalancerHealthCheck(%v,%v, %v, %v, %v)",
		loadBalancer, protocol, port, path, annotations)
	name := aws.StringValue(loadBalancer.LoadBalancerName)

	actual := loadBalancer.HealthCheck
	expectedTarget := protocol + ":" + strconv.FormatInt(int64(port), 10) + path
	expected, err := c.getExpectedHealthCheck(expectedTarget, annotations)
	if err != nil {
		return fmt.Errorf("cannot update health check for load balancer %q: %q", name, err)
	}

	// comparing attributes 1 by 1 to avoid breakage in case a new field is
	// added to the HC which breaks the equality
	if aws.StringValue(expected.Target) == aws.StringValue(actual.Target) &&
		aws.Int64Value(expected.HealthyThreshold) == aws.Int64Value(actual.HealthyThreshold) &&
		aws.Int64Value(expected.UnhealthyThreshold) == aws.Int64Value(actual.UnhealthyThreshold) &&
		aws.Int64Value(expected.Interval) == aws.Int64Value(actual.Interval) &&
		aws.Int64Value(expected.Timeout) == aws.Int64Value(actual.Timeout) {
		return nil
	}

	request := &elb.ConfigureHealthCheckInput{}
	request.HealthCheck = expected
	request.LoadBalancerName = loadBalancer.LoadBalancerName

	_, err = c.loadBalancer.ConfigureHealthCheck(request)
	if err != nil {
		return fmt.Errorf("error configuring load balancer health check for %q: %q", name, err)
	}

	return nil
}

// Makes sure that exactly the specified hosts are registered as instances with the load balancer
func (c *Cloud) ensureLoadBalancerInstances(loadBalancerName string,
	lbInstances []*elb.Instance,
	instanceIDs map[InstanceID]*osc.Vm) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("ensureLoadBalancerInstances(%v,%v, %v)", loadBalancerName, lbInstances, instanceIDs)
	expected := sets.NewString()
	for id := range instanceIDs {
		expected.Insert(string(id))
	}

	actual := sets.NewString()
	for _, lbInstance := range lbInstances {
		actual.Insert(aws.StringValue(lbInstance.InstanceId))
	}

	additions := expected.Difference(actual)
	removals := actual.Difference(expected)

	addInstances := []*elb.Instance{}
	for _, instanceID := range additions.List() {
		addInstance := &elb.Instance{}
		addInstance.InstanceId = aws.String(instanceID)
		addInstances = append(addInstances, addInstance)
	}

	removeInstances := []*elb.Instance{}
	for _, instanceID := range removals.List() {
		removeInstance := &elb.Instance{}
		removeInstance.InstanceId = aws.String(instanceID)
		removeInstances = append(removeInstances, removeInstance)
	}
	klog.V(5).Infof("ensureLoadBalancerInstances register/Deregister addInstances(%v) , removeInstances(%v)", addInstances, removeInstances)

	if len(addInstances) > 0 {
		registerRequest := &elb.RegisterInstancesWithLoadBalancerInput{}
		registerRequest.Instances = addInstances
		registerRequest.LoadBalancerName = aws.String(loadBalancerName)
		_, err := c.loadBalancer.RegisterInstancesWithLoadBalancer(registerRequest)
		if err != nil {
			return err
		}
		klog.V(1).Infof("Instances added to load-balancer %s", loadBalancerName)
	}

	if len(removeInstances) > 0 {
		deregisterRequest := &elb.DeregisterInstancesFromLoadBalancerInput{}
		deregisterRequest.Instances = removeInstances
		deregisterRequest.LoadBalancerName = aws.String(loadBalancerName)
		_, err := c.loadBalancer.DeregisterInstancesFromLoadBalancer(deregisterRequest)
		if err != nil {
			return err
		}
		klog.V(1).Infof("Instances removed from load-balancer %s", loadBalancerName)
	}

	return nil
}

func (c *Cloud) getLoadBalancerTLSPorts(loadBalancer *elb.LoadBalancerDescription) []int64 {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("getLoadBalancerTLSPorts(%v)", loadBalancer)
	ports := []int64{}

	for _, listenerDescription := range loadBalancer.ListenerDescriptions {
		protocol := aws.StringValue(listenerDescription.Listener.Protocol)
		if protocol == "SSL" || protocol == "HTTPS" {
			ports = append(ports, aws.Int64Value(listenerDescription.Listener.LoadBalancerPort))
		}
	}
	return ports
}

func (c *Cloud) ensureSSLNegotiationPolicy(loadBalancer *elb.LoadBalancerDescription, policyName string) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("ensureSSLNegotiationPolicy(%v,%v)", loadBalancer, policyName)
	klog.V(2).Info("Describing load balancer policies on load balancer")
	result, err := c.loadBalancer.DescribeLoadBalancerPolicies(&elb.DescribeLoadBalancerPoliciesInput{
		LoadBalancerName: loadBalancer.LoadBalancerName,
		PolicyNames: []*string{
			aws.String(fmt.Sprintf(SSLNegotiationPolicyNameFormat, policyName)),
		},
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case elb.ErrCodePolicyNotFoundException:
			default:
				return fmt.Errorf("error describing security policies on load balancer: %q", err)
			}
		}
	}

	if len(result.PolicyDescriptions) > 0 {
		return nil
	}

	klog.V(2).Infof("Creating SSL negotiation policy '%s' on load balancer", fmt.Sprintf(SSLNegotiationPolicyNameFormat, policyName))
	// there is an upper limit of 98 policies on an ELB, we're pretty safe from
	// running into it
	_, err = c.loadBalancer.CreateLoadBalancerPolicy(&elb.CreateLoadBalancerPolicyInput{
		LoadBalancerName: loadBalancer.LoadBalancerName,
		PolicyName:       aws.String(fmt.Sprintf(SSLNegotiationPolicyNameFormat, policyName)),
		PolicyTypeName:   aws.String("SSLNegotiationPolicyType"),
		PolicyAttributes: []*elb.PolicyAttribute{
			{
				AttributeName:  aws.String("Reference-Security-Policy"),
				AttributeValue: aws.String(policyName),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("error creating security policy on load balancer: %q", err)
	}
	return nil
}

func (c *Cloud) setSSLNegotiationPolicy(loadBalancerName, sslPolicyName string, port int64) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("setSSLNegotiationPolicy(%v,%v,%v)", loadBalancerName, sslPolicyName, port)
	policyName := fmt.Sprintf(SSLNegotiationPolicyNameFormat, sslPolicyName)
	request := &elb.SetLoadBalancerPoliciesOfListenerInput{
		LoadBalancerName: aws.String(loadBalancerName),
		LoadBalancerPort: aws.Int64(port),
		PolicyNames: []*string{
			aws.String(policyName),
		},
	}
	klog.V(2).Infof("Setting SSL negotiation policy '%s' on load balancer", policyName)
	_, err := c.loadBalancer.SetLoadBalancerPoliciesOfListener(request)
	if err != nil {
		return fmt.Errorf("error setting SSL negotiation policy '%s' on load balancer: %q", policyName, err)
	}
	return nil
}

func (c *Cloud) createProxyProtocolPolicy(loadBalancerName string, update bool) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("createProxyProtocolPolicy(%v) updating(%v)",
		loadBalancerName, update)
	request := &elb.CreateLoadBalancerPolicyInput{
		LoadBalancerName: aws.String(loadBalancerName),
		PolicyName:       aws.String(ProxyProtocolPolicyName),
		PolicyTypeName:   aws.String("ProxyProtocolPolicyType"),
		PolicyAttributes: []*elb.PolicyAttribute{
			{
				AttributeName:  aws.String("ProxyProtocol"),
				AttributeValue: aws.String("true"),
			},
		},
	}
	klog.V(2).Info("Creating proxy protocol policy on load balancer")
	_, err := c.loadBalancer.CreateLoadBalancerPolicy(request)
	if err != nil {
		if update {
			if aerr, ok := err.(awserr.Error); ok {
				if aerr.Code() == elb.ErrCodeDuplicatePolicyNameException {
					klog.V(2).Info("Updating proxy protocol policy on load balancer")
					return nil
				}
			}
		}
		return fmt.Errorf("error creating proxy protocol policy on load balancer: %q", err)
	}

	return nil
}

func (c *Cloud) setBackendPolicies(loadBalancerName string, instancePort int64, policies []*string) error {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("setBackendPolicies(%v,%v,%v)", loadBalancerName, instancePort, policies)
	request := &elb.SetLoadBalancerPoliciesForBackendServerInput{
		InstancePort:     aws.Int64(instancePort),
		LoadBalancerName: aws.String(loadBalancerName),
		PolicyNames:      policies,
	}
	if len(policies) > 0 {
		klog.V(2).Infof("Adding AWS loadbalancer backend policies on node port %d", instancePort)
	} else {
		klog.V(2).Infof("Removing AWS loadbalancer backend policies on node port %d", instancePort)
	}
	_, err := c.loadBalancer.SetLoadBalancerPoliciesForBackendServer(request)
	if err != nil {
		return fmt.Errorf("error adjusting AWS loadbalancer backend policies: %q", err)
	}

	return nil
}

func proxyProtocolEnabled(backend *elb.BackendServerDescription) bool {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("proxyProtocolEnabled(%v)", backend)
	for _, policy := range backend.PolicyNames {
		if aws.StringValue(policy) == ProxyProtocolPolicyName {
			return true
		}
	}

	return false
}

// findInstancesForELB gets the EC2 instances corresponding to the Nodes, for setting up an ELB
// We ignore Nodes (with a log message) where the instanceid cannot be determined from the provider,
// and we ignore instances which are not found
func (c *Cloud) findInstancesForELB(nodes []*v1.Node) (map[InstanceID]*osc.Vm, error) {
	debugPrintCallerFunctionName()
	klog.V(5).Infof("findInstancesForELB(%v)", nodes)

	for _, node := range nodes {
		if node.Spec.ProviderID == "" {
			// TODO  Need to be optimize by setting providerID which is not possible actualy
			instance, _ := c.findInstanceByNodeName(types.NodeName(node.Name))
			node.Spec.ProviderID = instance.GetVmId()
		}
	}

	// Map to instance ids ignoring Nodes where we cannot find the id (but logging)
	instanceIDs := mapToAWSInstanceIDsTolerant(nodes)

	cacheCriteria := cacheCriteria{
		// MaxAge not required, because we only care about security groups, which should not change
		HasInstances: instanceIDs, // Refresh if any of the instance ids are missing
	}
	snapshot, err := c.instanceCache.describeAllInstancesCached(cacheCriteria)
	if err != nil {
		return nil, err
	}

	instances := snapshot.FindInstances(instanceIDs)
	// We ignore instances that cannot be found

	return instances, nil
}
