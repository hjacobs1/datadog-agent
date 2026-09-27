// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	datadoghqcommon "github.com/DataDog/datadog-operator/api/datadoghq/common"

	"github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling"
)

// forcedResourcesInvalidReason is the ForcedResources condition reason when the annotation is ignored.
const forcedResourcesInvalidReason = "InvalidAnnotation"

// ResourceOverride holds the forced request and limit of one resource. A nil field is not
// forced and keeps the recommended value.
type ResourceOverride struct {
	Request *resource.Quantity
	Limit   *resource.Quantity
}

// ContainerResourcesOverride holds the forced values of one container, by resource name.
type ContainerResourcesOverride map[corev1.ResourceName]ResourceOverride

// ForcedResources holds the forced values set by the force-resources annotation, by container
// name. It is never mutated once parsed, so it can be shared between copies of the autoscaler.
type ForcedResources map[string]ContainerResourcesOverride

// forceResourcesField is the JSON shape of a single resource in the force-resources annotation.
type forceResourcesField struct {
	Request *string `json:"request"`
	Limit   *string `json:"limit"`
}

// parseForceResourcesAnnotation parses the force-resources annotation value. The annotation is
// all-or-nothing: any error makes it ignored as a whole, as partially applying an override meant
// as a unit would leave the workload in a state the operator did not ask for.
func parseForceResourcesAnnotation(value string) (ForcedResources, error) {
	if value == "" {
		return nil, nil
	}

	var raw map[string]map[corev1.ResourceName]forceResourcesField
	decoder := json.NewDecoder(bytes.NewReader([]byte(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("no container set")
	}

	forced := make(ForcedResources, len(raw))
	for containerName, resources := range raw {
		if containerName == "" {
			return nil, errors.New("empty container name")
		}
		if len(resources) == 0 {
			return nil, fmt.Errorf("container %q: no resource set", containerName)
		}

		override := make(ContainerResourcesOverride, len(resources))
		for resourceName, field := range resources {
			if resourceName != corev1.ResourceCPU && resourceName != corev1.ResourceMemory {
				return nil, fmt.Errorf("container %q: unsupported resource %q, only %q and %q are supported", containerName, resourceName, corev1.ResourceCPU, corev1.ResourceMemory)
			}

			request, err := parsePositiveQuantity(field.Request)
			if err != nil {
				return nil, fmt.Errorf("container %q: %s request: %w", containerName, resourceName, err)
			}
			limit, err := parsePositiveQuantity(field.Limit)
			if err != nil {
				return nil, fmt.Errorf("container %q: %s limit: %w", containerName, resourceName, err)
			}
			if request == nil && limit == nil {
				return nil, fmt.Errorf("container %q: %s has neither request nor limit", containerName, resourceName)
			}
			if request != nil && limit != nil && request.Cmp(*limit) > 0 {
				return nil, fmt.Errorf("container %q: %s request %s is greater than limit %s", containerName, resourceName, request, limit)
			}

			override[resourceName] = ResourceOverride{Request: request, Limit: limit}
		}
		forced[containerName] = override
	}

	return forced, nil
}

// parsePositiveQuantity parses an optional quantity, which must be strictly positive when set.
func parsePositiveQuantity(value *string) (*resource.Quantity, error) {
	if value == nil {
		return nil, nil
	}

	quantity, err := resource.ParseQuantity(*value)
	if err != nil {
		return nil, fmt.Errorf("invalid quantity %q: %w", *value, err)
	}
	if quantity.Sign() <= 0 {
		return nil, fmt.Errorf("quantity %q must be positive", *value)
	}

	return &quantity, nil
}

// ContainerNames returns the names of the containers with forced resources, sorted.
func (f ForcedResources) ContainerNames() []string {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// String returns a short human-readable description of the overridden containers.
func (f ForcedResources) String() string {
	return strings.Join(f.ContainerNames(), ", ")
}

// Apply overlays the forced values on the vertical scaling values, then recomputes their hash.
// Only forced fields are changed: other containers, resources, requests and limits keep their
// recommended value. Containers that have no recommendation are added with their forced values
// only, leaving everything else on the workload untouched.
//
// Apply is called on the active values and again after the vertical constraints are applied,
// so that forced values are never clamped or stripped: a break-glass override must not require
// editing the spec as well.
func (f ForcedResources) Apply(values *VerticalScalingValues) error {
	if values == nil || len(f) == 0 {
		return nil
	}

	applied := make(map[string]bool, len(f))
	for i := range values.ContainerResources {
		container := &values.ContainerResources[i]
		if override, found := f[container.Name]; found {
			override.applyTo(container)
			applied[container.Name] = true
		}
	}

	for _, name := range f.ContainerNames() {
		if applied[name] {
			continue
		}
		container := datadoghqcommon.DatadogPodAutoscalerContainerResources{Name: name}
		f[name].applyTo(&container)
		values.ContainerResources = append(values.ContainerResources, container)
	}

	hash, err := autoscaling.ObjectHash(values.ContainerResources)
	if err != nil {
		return fmt.Errorf("failed to compute resources hash after applying forced resources: %w", err)
	}
	values.ResourcesHash = hash
	return nil
}

// applyTo overlays the forced values on one container. When a forced value conflicts with a
// recommended one, the forced value wins and the recommended one is adjusted so that the request
// never exceeds the limit, which the API server would reject.
func (o ContainerResourcesOverride) applyTo(container *datadoghqcommon.DatadogPodAutoscalerContainerResources) {
	for resourceName, override := range o {
		if override.Request != nil {
			if container.Requests == nil {
				container.Requests = corev1.ResourceList{}
			}
			container.Requests[resourceName] = override.Request.DeepCopy()
		}
		if override.Limit != nil {
			if container.Limits == nil {
				container.Limits = corev1.ResourceList{}
			}
			container.Limits[resourceName] = override.Limit.DeepCopy()
		}

		request, hasRequest := container.Requests[resourceName]
		limit, hasLimit := container.Limits[resourceName]
		// A negative limit is the remove-limit sentinel, meaning no limit at all.
		if !hasRequest || !hasLimit || limit.Sign() < 0 || request.Cmp(limit) <= 0 {
			continue
		}
		if override.Limit != nil {
			container.Requests[resourceName] = limit.DeepCopy()
		} else {
			container.Limits[resourceName] = request.DeepCopy()
		}
	}
}
