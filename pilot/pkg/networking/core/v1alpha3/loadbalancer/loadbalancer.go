// Copyright 2019 Istio Authors
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

// packages used for load balancer setting
package loadbalancer

import (
	"math"
	"sort"

	apiv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/golang/protobuf/ptypes/wrappers"

	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/networking/util"
)

func GetLocalityLbSetting(
	mesh *v1alpha3.LocalityLoadBalancerSetting,
	destrule *v1alpha3.LocalityLoadBalancerSetting,
) *v1alpha3.LocalityLoadBalancerSetting {
	// Locality lb is enabled if its defined in mesh config
	enabled := mesh != nil
	// Unless we explicitly override this in destination rule
	if destrule != nil && destrule.Enabled != nil {
		enabled = destrule.Enabled.GetValue()
	}
	if !enabled {
		return nil
	}

	// Destination Rule overrides mesh config. If its defined, use that
	if destrule != nil {
		return destrule
	}
	// Otherwise fall back to mesh default
	return mesh
}

func ApplyLocalityLBSetting(
	locality *core.Locality,
	loadAssignment *apiv2.ClusterLoadAssignment,
	localityLB *v1alpha3.LocalityLoadBalancerSetting,
	enableFailover bool,
) {
	if locality == nil || loadAssignment == nil {
		return
	}

	// one of Distribute or Failover settings can be applied.
	if localityLB.GetDistribute() != nil {
		applyLocalityWeight(locality, loadAssignment, localityLB.GetDistribute())
	} else if enableFailover {
		// Failover needs outlier detection, otherwise Envoy will never drop down to a lower priority.
		applyLocalityFailover(locality, loadAssignment, localityLB.GetFailover())
	}
}

// set locality loadbalancing weight
func applyLocalityWeight(
	locality *core.Locality,
	loadAssignment *apiv2.ClusterLoadAssignment,
	distribute []*v1alpha3.LocalityLoadBalancerSetting_Distribute) {
	if distribute == nil {
		return
	}

	// Support Locality weighted load balancing
	// (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/load_balancing/locality_weight.html)
	// by providing weights in LocalityLbEndpoints via load_balancing_weight.
	// By setting weights across different localities, it can allow
	// Envoy to weight assignments across different zones and geographical locations.
	for _, localityWeightSetting := range distribute {
		if localityWeightSetting != nil &&
			util.LocalityMatch(locality, localityWeightSetting.From) {
			misMatched := map[int]struct{}{}
			for i := range loadAssignment.Endpoints {
				misMatched[i] = struct{}{}
			}
			for locality, weight := range localityWeightSetting.To {
				// index -> original weight
				destLocMap := map[int]uint32{}
				totalWeight := uint32(0)
				for i, ep := range loadAssignment.Endpoints {
					if _, exist := misMatched[i]; exist {
						if util.LocalityMatch(ep.Locality, locality) {
							delete(misMatched, i)
							if ep.LoadBalancingWeight != nil {
								destLocMap[i] = ep.LoadBalancingWeight.Value
							} else {
								destLocMap[i] = 1
							}
							totalWeight += destLocMap[i]
						}
					}
				}
				// in case wildcard dest matching multi groups of endpoints
				// the load balancing weight for a locality is divided by the sum of the weights of all localities
				for index, originalWeight := range destLocMap {
					destWeight := float64(originalWeight*weight) / float64(totalWeight)
					if destWeight > 0 {
						loadAssignment.Endpoints[index].LoadBalancingWeight = &wrappers.UInt32Value{
							Value: uint32(math.Ceil(destWeight)),
						}
					}
				}
			}

			// remove groups of endpoints in a locality that miss matched
			for i := range misMatched {
				loadAssignment.Endpoints[i].LbEndpoints = nil
			}
			break
		}
	}
}

// set locality loadbalancing priority
func applyLocalityFailover(
	locality *core.Locality,
	loadAssignment *apiv2.ClusterLoadAssignment,
	failover []*v1alpha3.LocalityLoadBalancerSetting_Failover) {
	// key is priority, value is the index of the LocalityLbEndpoints in ClusterLoadAssignment
	priorityMap := map[int][]int{}

	// 1. calculate the LocalityLbEndpoints.Priority compared with proxy locality
	for i, localityEndpoint := range loadAssignment.Endpoints {
		// if region/zone/subZone all match, the priority is 0.
		// if region/zone match, the priority is 1.
		// if region matches, the priority is 2.
		// if locality not match, the priority is 3.
		priority := util.LbPriority(locality, localityEndpoint.Locality)
		// region not match, apply failover settings when specified
		// update localityLbEndpoints' priority to 4 if failover not match
		if priority == 3 {
			for _, failoverSetting := range failover {
				if failoverSetting.From == locality.Region {
					if localityEndpoint.Locality == nil || localityEndpoint.Locality.Region != failoverSetting.To {
						priority = 4
					}
					break
				}
			}
		}
		loadAssignment.Endpoints[i].Priority = uint32(priority)
		priorityMap[priority] = append(priorityMap[priority], i)
	}

	// since Priorities should range from 0 (highest) to N (lowest) without skipping.
	// 2. adjust the priorities in order
	// 2.1 sort all priorities in increasing order.
	priorities := []int{}
	for priority := range priorityMap {
		priorities = append(priorities, priority)
	}
	sort.Ints(priorities)
	// 2.2 adjust LocalityLbEndpoints priority
	// if the index and value of priorities array is not equal.
	for i, priority := range priorities {
		if i != priority {
			// the LocalityLbEndpoints index in ClusterLoadAssignment.Endpoints
			for _, index := range priorityMap[priority] {
				loadAssignment.Endpoints[index].Priority = uint32(i)
			}
		}
	}

}
