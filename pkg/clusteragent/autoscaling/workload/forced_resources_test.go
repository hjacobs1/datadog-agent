// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package workload

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	datadoghqcommon "github.com/DataDog/datadog-operator/api/datadoghq/common"
	datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha2"

	autoscalingstore "github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling/store"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling/workload/model"
	"github.com/DataDog/datadog-agent/pkg/util/pointer"
)

// newForcedResourcesAutoscaler builds an autoscaler whose recommendation is 500m CPU and 256Mi
// memory for container1, with the given annotations and spec, and runs the source selection the
// controller runs on every sync.
func newForcedResourcesAutoscaler(annotations map[string]string, spec *datadoghq.DatadogPodAutoscalerSpec) model.PodAutoscalerInternal {
	if spec == nil {
		spec = &datadoghq.DatadogPodAutoscalerSpec{}
	}
	spec.TargetRef = autoscalingv2.CrossVersionObjectReference{
		Kind:       "Deployment",
		APIVersion: "apps/v1",
		Name:       "test-deployment",
	}

	pai := model.FakePodAutoscalerInternal{
		Namespace: "ns1",
		Name:      "autoscaler1",
		Spec:      spec,
		MainScalingValues: model.ScalingValues{
			Vertical: &model.VerticalScalingValues{
				Source:        datadoghqcommon.DatadogPodAutoscalerAutoscalingValueSource,
				Timestamp:     time.Now().Add(-time.Minute),
				ResourcesHash: "version1",
				ContainerResources: []datadoghqcommon.DatadogPodAutoscalerContainerResources{{
					Name:     "container1",
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				}},
			},
		},
	}.Build()
	pai.UpdateOpsAnnotations(annotations)

	horizontalSource, verticalSource := getActiveScalingSources(time.Now(), &pai)
	pai.SetActiveScalingValues(time.Now(), horizontalSource, verticalSource)
	return pai
}

func newForcedResourcesPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pod1",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "test-deployment-968f49d86",
				APIVersion: "apps/v1",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "container1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
}

func applyWithPatcher(t *testing.T, pai model.PodAutoscalerInternal) *corev1.Pod {
	t.Helper()
	s := autoscalingstore.NewStore[model.PodAutoscalerInternal]()
	item, _ := s.Get(pai.ID())
	item.Upsert(pai, "")

	pod := newForcedResourcesPod()
	_, err := NewPodPatcher(s, nil, nil).ApplyRecommendations(pod)
	require.NoError(t, err)
	return pod
}

// TestPatcherApplyForcedResources covers the admission webhook, which re-derives constraints on
// every replica: forced values must reach new pods unclamped, while the values that keep following
// the recommendation are still constrained.
func TestPatcherApplyForcedResources(t *testing.T) {
	constraints := &datadoghqcommon.DatadogPodAutoscalerConstraints{
		Containers: []datadoghqcommon.DatadogPodAutoscalerContainerConstraints{{
			Name:       "container1",
			MaxAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		}},
	}
	const annotation = `{"container1": {"cpu": {"request": "2"}}}`

	t.Run("forced values are not clamped, recommended ones still are", func(t *testing.T) {
		pai := newForcedResourcesAutoscaler(map[string]string{model.ForceResourcesAnnotationKey: annotation}, &datadoghq.DatadogPodAutoscalerSpec{Constraints: constraints})
		pod := applyWithPatcher(t, pai)

		requests := pod.Spec.Containers[0].Resources.Requests
		assert.Equal(t, "2", requests.Cpu().String(), "a forced value wins over maxAllowed")
		assert.Equal(t, "128Mi", requests.Memory().String(), "a recommended value is still clamped")
		assert.Equal(t, pai.ScalingValues().Vertical.ResourcesHash, pod.Annotations[model.RecommendationIDAnnotation])
	})

	t.Run("without the annotation the recommendation is clamped", func(t *testing.T) {
		pod := applyWithPatcher(t, newForcedResourcesAutoscaler(nil, &datadoghq.DatadogPodAutoscalerSpec{Constraints: constraints}))

		assert.Equal(t, "500m", pod.Spec.Containers[0].Resources.Requests.Cpu().String())
	})

	t.Run("Preview suppresses the override", func(t *testing.T) {
		spec := &datadoghq.DatadogPodAutoscalerSpec{
			ApplyPolicy: &datadoghq.DatadogPodAutoscalerApplyPolicy{Mode: datadoghq.DatadogPodAutoscalerApplyModePreview},
		}
		pod := applyWithPatcher(t, newForcedResourcesAutoscaler(map[string]string{model.ForceResourcesAnnotationKey: annotation}, spec))

		assert.Equal(t, "100m", pod.Spec.Containers[0].Resources.Requests.Cpu().String())
	})

	t.Run("pause suppresses the override", func(t *testing.T) {
		pod := applyWithPatcher(t, newForcedResourcesAutoscaler(map[string]string{model.ForceResourcesAnnotationKey: annotation, model.PauseAnnotationKey: "true"}, nil))

		assert.Equal(t, "100m", pod.Spec.Containers[0].Resources.Requests.Cpu().String())
	})
}

// TestVerticalConstraintsForcedResources covers the vertical controller path: constraints, then
// forced values, as done before deciding on an in-place resize or a rollout.
func TestVerticalConstraintsForcedResources(t *testing.T) {
	spec := &datadoghq.DatadogPodAutoscalerSpec{
		Constraints: &datadoghqcommon.DatadogPodAutoscalerConstraints{
			Containers: []datadoghqcommon.DatadogPodAutoscalerContainerConstraints{{
				Name:       "container1",
				MaxAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}},
		},
		Options: &datadoghqcommon.DatadogPodAutoscalerOptions{Burstable: pointer.Ptr(true)},
	}

	for _, tt := range []struct {
		name          string
		annotation    string
		expectedCPU   string
		expectedLimit string
	}{
		{name: "forced request, burstable still removes the limit", annotation: `{"container1": {"cpu": {"request": "2"}}}`, expectedCPU: "2", expectedLimit: "-1"},
		{name: "forced limit wins over burstable", annotation: `{"container1": {"cpu": {"request": "2", "limit": "3"}}}`, expectedCPU: "2", expectedLimit: "3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pai := newForcedResourcesAutoscaler(map[string]string{model.ForceResourcesAnnotationKey: tt.annotation}, spec.DeepCopy())

			constrained := pai.ScalingValues().Vertical.DeepCopy()
			_, err := applyVerticalConstraints(constrained, pai.Spec().Constraints, pai.IsBurstable())
			require.NoError(t, err)
			require.NoError(t, pai.ReapplyForcedResources(constrained))

			container := constrained.ContainerResources[0]
			assert.Equal(t, tt.expectedCPU, container.Requests.Cpu().String(), "not clamped by maxAllowed")
			assert.Equal(t, tt.expectedLimit, container.Limits.Cpu().String())
		})
	}
}

// TestGetVerticalPatchingStrategyForcedResources pins that forced resources are rolled out exactly
// like a recommendation: the strategy never depends on the source, so the in-place resize,
// eviction and rollout decisions made from it are the same.
func TestGetVerticalPatchingStrategyForcedResources(t *testing.T) {
	for _, applyPolicy := range []*datadoghq.DatadogPodAutoscalerApplyPolicy{
		nil,
		{Mode: datadoghq.DatadogPodAutoscalerApplyModeApply},
		{Mode: datadoghq.DatadogPodAutoscalerApplyModeApply, Update: &datadoghqcommon.DatadogPodAutoscalerUpdatePolicy{Strategy: datadoghqcommon.DatadogPodAutoscalerTriggerRolloutUpdateStrategy}},
		{Mode: datadoghq.DatadogPodAutoscalerApplyModeApply, Update: &datadoghqcommon.DatadogPodAutoscalerUpdatePolicy{Strategy: datadoghqcommon.DatadogPodAutoscalerDisabledUpdateStrategy}},
		{Mode: datadoghq.DatadogPodAutoscalerApplyModePreview},
	} {
		recommended := newForcedResourcesAutoscaler(nil, &datadoghq.DatadogPodAutoscalerSpec{ApplyPolicy: applyPolicy})
		forced := newForcedResourcesAutoscaler(map[string]string{model.ForceResourcesAnnotationKey: `{"container1": {"cpu": {"request": "2"}}}`}, &datadoghq.DatadogPodAutoscalerSpec{ApplyPolicy: applyPolicy})
		require.Equal(t, datadoghqcommon.DatadogPodAutoscalerManualValueSource, forced.ScalingValues().Vertical.Source)

		recommendedStrategy, _ := getVerticalPatchingStrategy(&recommended)
		forcedStrategy, _ := getVerticalPatchingStrategy(&forced)
		assert.Equal(t, recommendedStrategy, forcedStrategy, "apply policy %+v", applyPolicy)
	}
}

// TestPatcherApplyForcedResourcesMultipleContainers covers an annotation that overrides several
// containers at once, each with different fields, next to a container it does not list and one
// that has no recommendation at all.
func TestPatcherApplyForcedResourcesMultipleContainers(t *testing.T) {
	pai := model.FakePodAutoscalerInternal{
		Namespace: "ns1",
		Name:      "autoscaler1",
		Spec: &datadoghq.DatadogPodAutoscalerSpec{
			TargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", APIVersion: "apps/v1", Name: "test-deployment"},
			Constraints: &datadoghqcommon.DatadogPodAutoscalerConstraints{
				Containers: []datadoghqcommon.DatadogPodAutoscalerContainerConstraints{{
					Name:       "*",
					MaxAllowed: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}},
			},
		},
		MainScalingValues: model.ScalingValues{
			Vertical: &model.VerticalScalingValues{
				Source:        datadoghqcommon.DatadogPodAutoscalerAutoscalingValueSource,
				Timestamp:     time.Now().Add(-time.Minute),
				ResourcesHash: "version1",
				ContainerResources: []datadoghqcommon.DatadogPodAutoscalerContainerResources{
					{
						Name:     "app",
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
					{
						Name:     "sidecar",
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
					},
					{
						Name:     "logger",
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
					},
				},
			},
		},
	}.Build()
	pai.UpdateOpsAnnotations(map[string]string{model.ForceResourcesAnnotationKey: `{
		"app":     {"cpu": {"request": "2"}},
		"sidecar": {"memory": {"limit": "512Mi"}},
		"worker":  {"cpu": {"request": "300m", "limit": "600m"}, "memory": {"request": "1Gi"}}
	}`})
	require.NotNil(t, pai.ForcedResources(), "a multi-container annotation must parse")
	assert.Equal(t, []string{"app", "sidecar", "worker"}, pai.ForcedResources().ContainerNames())
	horizontalSource, verticalSource := getActiveScalingSources(time.Now(), &pai)
	pai.SetActiveScalingValues(time.Now(), horizontalSource, verticalSource)

	s := autoscalingstore.NewStore[model.PodAutoscalerInternal]()
	item, _ := s.Get(pai.ID())
	item.Upsert(pai, "")

	pod := newForcedResourcesPod()
	pod.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: "sidecar"}, {Name: "logger"}, {Name: "worker"}}
	_, err := NewPodPatcher(s, nil, nil).ApplyRecommendations(pod)
	require.NoError(t, err)

	resources := map[string]*corev1.ResourceRequirements{}
	for i := range pod.Spec.Containers {
		resources[pod.Spec.Containers[i].Name] = &pod.Spec.Containers[i].Resources
	}

	assert.Equal(t, "2", resources["app"].Requests.Cpu().String(), "app: forced cpu request, not clamped")
	assert.Equal(t, "256Mi", resources["app"].Requests.Memory().String(), "app: memory keeps its recommendation")

	assert.Equal(t, "512Mi", resources["sidecar"].Limits.Memory().String(), "sidecar: forced memory limit")
	assert.Equal(t, "64Mi", resources["sidecar"].Requests.Memory().String(), "sidecar: memory request keeps its recommendation")
	assert.Equal(t, "100m", resources["sidecar"].Requests.Cpu().String(), "sidecar: cpu keeps its recommendation")

	assert.Equal(t, "50m", resources["logger"].Requests.Cpu().String(), "logger: unlisted, keeps its recommendation")

	assert.Equal(t, "300m", resources["worker"].Requests.Cpu().String(), "worker: forced without any recommendation")
	assert.Equal(t, "600m", resources["worker"].Limits.Cpu().String())
	assert.Equal(t, "1Gi", resources["worker"].Requests.Memory().String())
	_, hasMemoryLimit := resources["worker"].Limits[corev1.ResourceMemory]
	assert.False(t, hasMemoryLimit, "worker: a limit neither forced nor recommended is left untouched")
}
