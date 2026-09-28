// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package ecs

import (
	"context"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
)

// maxStoppedTasksPerService bounds how many stopped tasks are described for a
// service: a crashlooping service accumulates them indefinitely, and the most
// recent few always carry the same reason.
const maxStoppedTasksPerService = 3

// maxServiceEvents bounds the per-service event tail. ECS keeps a long history
// and only the newest entries describe the current failure.
const maxServiceEvents = 5

// DumpECSClusterState reports why an ECS cluster's services are not running.
//
// A task that never starts is invisible everywhere else: services are created
// with ContinueBeforeSteadyState, so Pulumi reports the stack provisioned while
// the task crashloops, and a test waiting on telemetry just sees an empty
// intake. The stopCode/stoppedReason printed here (a failed image pull, an
// execution-role problem) names the cause directly.
func DumpECSClusterState(ctx context.Context, stackName string) (string, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to load AWS config: %w", err)
	}
	client := awsecs.NewFromConfig(cfg)

	clusterArn, err := findClusterForStack(ctx, client, stackName)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	fmt.Fprintf(&out, "ECS cluster: %s\n", clusterArn)

	services, err := client.ListServices(ctx, &awsecs.ListServicesInput{Cluster: &clusterArn})
	if err != nil {
		return "", fmt.Errorf("failed to list services of %s: %w", clusterArn, err)
	}
	if len(services.ServiceArns) == 0 {
		fmt.Fprintf(&out, "  no services\n")
		return out.String(), nil
	}

	described, err := client.DescribeServices(ctx, &awsecs.DescribeServicesInput{
		Cluster:  &clusterArn,
		Services: services.ServiceArns,
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe services of %s: %w", clusterArn, err)
	}

	for _, svc := range described.Services {
		name := derefOr(svc.ServiceName, "<unnamed>")
		fmt.Fprintf(&out, "\n  service %s: running=%d desired=%d\n", name, svc.RunningCount, svc.DesiredCount)

		for i, ev := range svc.Events {
			if i >= maxServiceEvents {
				break
			}
			fmt.Fprintf(&out, "    event: %s\n", derefOr(ev.Message, ""))
		}

		// Only dig into stopped tasks when the service is short of its target:
		// a healthy service's old stopped tasks are noise.
		if svc.RunningCount >= svc.DesiredCount {
			continue
		}
		stopped, err := dumpStoppedTasks(ctx, client, clusterArn, name)
		if err != nil {
			fmt.Fprintf(&out, "    (could not describe stopped tasks: %v)\n", err)
			continue
		}
		out.WriteString(stopped)
	}

	return out.String(), nil
}

// dumpStoppedTasks reports the stop reason of a service's most recent stopped
// tasks, which is where a task that failed to start explains itself.
func dumpStoppedTasks(ctx context.Context, client *awsecs.Client, clusterArn, serviceName string) (string, error) {
	listed, err := client.ListTasks(ctx, &awsecs.ListTasksInput{
		Cluster:       &clusterArn,
		ServiceName:   &serviceName,
		DesiredStatus: "STOPPED",
	})
	if err != nil {
		return "", err
	}
	if len(listed.TaskArns) == 0 {
		return "    no stopped tasks\n", nil
	}

	// ListTasks returns oldest first, and the newest failures are the relevant ones.
	arns := listed.TaskArns
	if len(arns) > maxStoppedTasksPerService {
		arns = arns[len(arns)-maxStoppedTasksPerService:]
	}

	described, err := client.DescribeTasks(ctx, &awsecs.DescribeTasksInput{
		Cluster: &clusterArn,
		Tasks:   arns,
	})
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, task := range described.Tasks {
		fmt.Fprintf(&out, "    stopped task: stopCode=%s reason=%s\n",
			task.StopCode, derefOr(task.StoppedReason, ""))
		for _, c := range task.Containers {
			// A container that never started has no exit code; its own reason
			// field is where the pull or registry error lands.
			fmt.Fprintf(&out, "      container %s: exitCode=%s reason=%s\n",
				derefOr(c.Name, "<unnamed>"), formatExitCode(c.ExitCode), derefOr(c.Reason, ""))
		}
	}
	return out.String(), nil
}

// findClusterForStack matches a cluster to the stack that created it. The
// cluster name embeds the stack name but is not equal to it (the scenario
// appends its own suffix), so this matches on containment rather than equality.
func findClusterForStack(ctx context.Context, client *awsecs.Client, stackName string) (string, error) {
	listed, err := client.ListClusters(ctx, &awsecs.ListClustersInput{})
	if err != nil {
		return "", fmt.Errorf("failed to list ECS clusters: %w", err)
	}

	// The stack name reaches ECS lowercased, and may be fully qualified
	// (organization/project/stack) while the cluster name uses only the last part.
	needle := strings.ToLower(stackName)
	if idx := strings.LastIndex(needle, "/"); idx >= 0 {
		needle = needle[idx+1:]
	}

	for _, arn := range listed.ClusterArns {
		if strings.Contains(strings.ToLower(arn), needle) {
			return arn, nil
		}
	}
	return "", fmt.Errorf("no ECS cluster found for stack %s among %d cluster(s)", stackName, len(listed.ClusterArns))
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func formatExitCode(code *int32) string {
	if code == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *code)
}
