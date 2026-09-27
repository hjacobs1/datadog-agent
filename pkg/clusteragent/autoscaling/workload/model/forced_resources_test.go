// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	datadoghqcommon "github.com/DataDog/datadog-operator/api/datadoghq/common"

	"github.com/DataDog/datadog-agent/pkg/util/pointer"
)

func TestParseForceResourcesAnnotation(t *testing.T) {
	forced, err := parseForceResourcesAnnotation(`{"app": {"cpu": {"request": "2", "limit": "4"}, "memory": {"request": "200Mi"}}, "sidecar": {"memory": {"limit": "1Gi"}}}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"app", "sidecar"}, forced.ContainerNames())
	assert.True(t, forced["app"][corev1.ResourceCPU].Request.Equal(resource.MustParse("2")))
	assert.True(t, forced["app"][corev1.ResourceCPU].Limit.Equal(resource.MustParse("4")))
	assert.True(t, forced["app"][corev1.ResourceMemory].Request.Equal(resource.MustParse("200Mi")))
	assert.Nil(t, forced["app"][corev1.ResourceMemory].Limit, "a limit that is not set is not forced")
	assert.Nil(t, forced["sidecar"][corev1.ResourceMemory].Request)

	forced, err = parseForceResourcesAnnotation("")
	assert.NoError(t, err, "an absent annotation is not an error")
	assert.Nil(t, forced)

	for name, value := range map[string]string{
		"bad JSON":                   `{"app": `,
		"not an object":              `["app"]`,
		"no container":               `{}`,
		"empty container name":       `{"": {"cpu": {"request": "1"}}}`,
		"no resource":                `{"app": {}}`,
		"unsupported resource":       `{"app": {"nvidia.com/gpu": {"request": "1"}}}`,
		"unknown field":              `{"app": {"cpu": {"requests": "1"}}}`,
		"neither request nor limit":  `{"app": {"cpu": {}}}`,
		"invalid quantity":           `{"app": {"memory": {"request": "200Mb"}}}`,
		"zero quantity":              `{"app": {"cpu": {"request": "0"}}}`,
		"negative quantity":          `{"app": {"cpu": {"limit": "-1"}}}`,
		"request greater than limit": `{"app": {"cpu": {"request": "4", "limit": "2"}}}`,
		"one invalid container":      `{"app": {"cpu": {"request": "1"}}, "other": {"memory": {"request": "200Mb"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			forced, err := parseForceResourcesAnnotation(value)
			assert.Error(t, err)
			assert.Nil(t, forced, "an invalid annotation is ignored as a whole")
		})
	}
}

func TestForcedResourcesApply(t *testing.T) {
	recommendation := func() *VerticalScalingValues {
		return &VerticalScalingValues{
			Source:        datadoghqcommon.DatadogPodAutoscalerAutoscalingValueSource,
			ResourcesHash: "recommendation-hash",
			ContainerResources: []datadoghqcommon.DatadogPodAutoscalerContainerResources{
				{
					Name:     "app",
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
				{
					Name:     "sidecar",
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			},
		}
	}
	mustParse := func(t *testing.T, value string) ForcedResources {
		forced, err := parseForceResourcesAnnotation(value)
		require.NoError(t, err)
		return forced
	}

	t.Run("only the forced fields are overridden", func(t *testing.T) {
		values := recommendation()
		require.NoError(t, mustParse(t, `{"app": {"cpu": {"request": "750m"}}}`).Apply(values))

		app := values.ContainerResources[0]
		assert.True(t, app.Requests[corev1.ResourceCPU].Equal(resource.MustParse("750m")), "forced request")
		assert.True(t, app.Limits[corev1.ResourceCPU].Equal(resource.MustParse("1")), "cpu limit keeps its recommendation")
		assert.True(t, app.Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")), "memory keeps its recommendation")
		assert.True(t, app.Limits[corev1.ResourceMemory].Equal(resource.MustParse("512Mi")), "memory keeps its recommendation")
		assert.Equal(t, recommendation().ContainerResources[1], values.ContainerResources[1], "an unlisted container keeps its recommendation")
		assert.NotEqual(t, "recommendation-hash", values.ResourcesHash, "the hash reflects the forced values")
	})

	t.Run("a container without recommendation gets only its forced values", func(t *testing.T) {
		values := recommendation()
		require.NoError(t, mustParse(t, `{"worker": {"memory": {"limit": "1Gi"}}}`).Apply(values))

		require.Len(t, values.ContainerResources, 3)
		worker := values.ContainerResources[2]
		assert.Equal(t, "worker", worker.Name)
		assert.Empty(t, worker.Requests, "values that are neither forced nor recommended are left untouched on the workload")
		assert.Equal(t, corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}, worker.Limits)
	})

	t.Run("no recommendation at all", func(t *testing.T) {
		values := &VerticalScalingValues{}
		require.NoError(t, mustParse(t, `{"app": {"cpu": {"request": "2", "limit": "4"}}}`).Apply(values))

		require.Len(t, values.ContainerResources, 1)
		assert.NotEmpty(t, values.ResourcesHash, "a hash is required for the vertical controller to act")
	})

	t.Run("a forced request above the recommended limit raises the limit", func(t *testing.T) {
		values := recommendation()
		require.NoError(t, mustParse(t, `{"app": {"cpu": {"request": "2"}}}`).Apply(values))

		assert.True(t, values.ContainerResources[0].Limits[corev1.ResourceCPU].Equal(resource.MustParse("2")))
	})

	t.Run("a forced limit below the recommended request lowers the request", func(t *testing.T) {
		values := recommendation()
		require.NoError(t, mustParse(t, `{"app": {"memory": {"limit": "128Mi"}}}`).Apply(values))

		assert.True(t, values.ContainerResources[0].Requests[corev1.ResourceMemory].Equal(resource.MustParse("128Mi")))
		assert.True(t, values.ContainerResources[0].Limits[corev1.ResourceMemory].Equal(resource.MustParse("128Mi")))
	})

	t.Run("the remove-limit sentinel is kept unless the limit is forced", func(t *testing.T) {
		values := recommendation()
		values.ContainerResources[0].Limits[corev1.ResourceCPU] = resource.MustParse("-1")
		require.NoError(t, mustParse(t, `{"app": {"cpu": {"request": "2"}}}`).Apply(values))
		assert.True(t, values.ContainerResources[0].Limits[corev1.ResourceCPU].Equal(resource.MustParse("-1")), "burstable removes the limit")

		require.NoError(t, mustParse(t, `{"app": {"cpu": {"limit": "4"}}}`).Apply(values))
		assert.True(t, values.ContainerResources[0].Limits[corev1.ResourceCPU].Equal(resource.MustParse("4")), "a forced limit wins over burstable")
	})

	t.Run("applying twice is a no-op", func(t *testing.T) {
		forced := mustParse(t, `{"app": {"cpu": {"request": "2"}}, "worker": {"memory": {"limit": "1Gi"}}}`)
		once := recommendation()
		require.NoError(t, forced.Apply(once))
		twice := once.DeepCopy()
		require.NoError(t, forced.Apply(twice))

		assert.Equal(t, once.ResourcesHash, twice.ResourcesHash)
		assert.Equal(t, once.ContainerResources, twice.ContainerResources)
	})
}

func TestSetActiveScalingValuesForcedResources(t *testing.T) {
	currentTime := time.Now()
	const annotation = `{"app": {"cpu": {"request": "2"}}}`
	mainVertical := &VerticalScalingValues{
		Source:        datadoghqcommon.DatadogPodAutoscalerAutoscalingValueSource,
		Timestamp:     currentTime.Add(-time.Minute),
		ResourcesHash: "main-hash",
		ContainerResources: []datadoghqcommon.DatadogPodAutoscalerContainerResources{{
			Name:     "app",
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}},
	}
	newAutoscaler := func(annotations map[string]string, main *VerticalScalingValues) PodAutoscalerInternal {
		pai := PodAutoscalerInternal{}
		pai.UpdateOpsAnnotations(annotations)
		pai.UpdateFromMainValues(ScalingValues{Vertical: main}, 1)
		return pai
	}
	activeSource := func(pai PodAutoscalerInternal) *datadoghqcommon.DatadogPodAutoscalerValueSource {
		if pai.MainScalingValues().Vertical == nil {
			return nil
		}
		return pointer.Ptr(pai.MainScalingValues().Vertical.Source)
	}

	t.Run("forced values are overlaid on the recommendation", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation}, mainVertical)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		active := pai.ScalingValues().Vertical
		require.NotNil(t, active)
		assert.Equal(t, datadoghqcommon.DatadogPodAutoscalerManualValueSource, active.Source)
		assert.Equal(t, mainVertical.Timestamp, active.Timestamp, "the recommendation timestamp is kept")
		assert.True(t, active.ContainerResources[0].Requests[corev1.ResourceCPU].Equal(resource.MustParse("2")))
		assert.True(t, active.ContainerResources[0].Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
		assert.True(t, pai.MainScalingValues().Vertical.ContainerResources[0].Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")),
			"the recommendation itself is not modified")
	})

	t.Run("forced values apply without any recommendation", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation}, nil)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		active := pai.ScalingValues().Vertical
		require.NotNil(t, active)
		assert.NotEmpty(t, active.ResourcesHash)
		assert.False(t, active.Timestamp.IsZero())
	})

	t.Run("the active values are stable across syncs", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation}, nil)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))
		first := pai.ScalingValues().Vertical.DeepCopy()

		pai.UpdateOpsAnnotations(map[string]string{ForceResourcesAnnotationKey: annotation})
		pai.SetActiveScalingValues(currentTime.Add(time.Minute), nil, activeSource(pai))

		assert.Equal(t, first, pai.ScalingValues().Vertical, "a changing hash or timestamp would trigger rollouts and status updates on every sync")
	})

	t.Run("a Manual vertical recommendation from remote config is overlaid too", func(t *testing.T) {
		manual := mainVertical.DeepCopy()
		manual.Source = datadoghqcommon.DatadogPodAutoscalerManualValueSource
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation}, manual)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		assert.True(t, pai.ScalingValues().Vertical.ContainerResources[0].Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
	})

	t.Run("pause wins", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation, PauseAnnotationKey: "true"}, mainVertical)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		assert.Equal(t, "main-hash", pai.ScalingValues().Vertical.ResourcesHash)
		assert.NoError(t, pai.ReapplyForcedResources(pai.ScalingValues().Vertical))
		assert.Equal(t, "main-hash", pai.ScalingValues().Vertical.ResourcesHash, "reapplying is a no-op while paused")
	})

	t.Run("removing the annotation returns to the recommendation", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: annotation}, mainVertical)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))
		pai.UpdateOpsAnnotations(map[string]string{})
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		assert.Nil(t, pai.ForcedResources())
		assert.Equal(t, "main-hash", pai.ScalingValues().Vertical.ResourcesHash)
	})

	t.Run("an invalid annotation is ignored", func(t *testing.T) {
		pai := newAutoscaler(map[string]string{ForceResourcesAnnotationKey: `{"app": {"memory": {"request": "200Mb"}}}`}, mainVertical)
		pai.SetActiveScalingValues(currentTime, nil, activeSource(pai))

		assert.Nil(t, pai.ForcedResources())
		assert.Equal(t, "main-hash", pai.ScalingValues().Vertical.ResourcesHash)
	})
}

func TestBuildStatusForcedResourcesCondition(t *testing.T) {
	findCondition := func(status datadoghqcommon.DatadogPodAutoscalerStatus) *datadoghqcommon.DatadogPodAutoscalerCondition {
		for i := range status.Conditions {
			if status.Conditions[i].Type == DatadogPodAutoscalerForcedResourcesCondition {
				return &status.Conditions[i]
			}
		}
		return nil
	}
	pai := FakePodAutoscalerInternal{Namespace: "ns", Name: "dpa"}.Build()

	assert.Nil(t, findCondition(pai.BuildStatus(metav1.Now(), nil)), "not surfaced without the annotation")

	pai.UpdateOpsAnnotations(map[string]string{ForceResourcesAnnotationKey: `{"worker": {"cpu": {"request": "1"}}, "app": {"cpu": {"request": "2"}}}`})
	condition := findCondition(pai.BuildStatus(metav1.Now(), nil))
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Contains(t, condition.Message, "app, worker")

	pai.UpdateOpsAnnotations(map[string]string{ForceResourcesAnnotationKey: `{"app": {"cpu": {"request": "4", "limit": "2"}}}`})
	condition = findCondition(pai.BuildStatus(metav1.Now(), nil))
	require.NotNil(t, condition, "an ignored annotation must be visible to the operator who set it")
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, forcedResourcesInvalidReason, condition.Reason)
	assert.Contains(t, condition.Message, "greater than limit")

	pai.UpdateOpsAnnotations(map[string]string{})
	assert.Nil(t, findCondition(pai.BuildStatus(metav1.Now(), nil)), "cleared when the annotation is removed")
}
