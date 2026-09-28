// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package aas wires the Azure App Service (.NET extension) dogstatsd.exe
// process to the shared inventoryagent component so it can emit a serverless
// inventory payload. All AAS-specific metadata derivation lives here; the
// shared component stays generic.
package aas

import (
	"context"
	"os"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/fx"

	coreconfig "github.com/DataDog/datadog-agent/comp/core/config"
	inventoryagent "github.com/DataDog/datadog-agent/comp/metadata/inventoryagent/def"
	metadatarunner "github.com/DataDog/datadog-agent/comp/metadata/runner/def"
	configmodel "github.com/DataDog/datadog-agent/pkg/config/model"
	serverlessenv "github.com/DataDog/datadog-agent/pkg/serverless/env"
	"github.com/DataDog/datadog-agent/pkg/trace/traceutil"
)

const aasInventoryFlavor = "serverless-extension"

const (
	reportReasonStartup  = "startup"
	reportReasonPeriodic = "periodic"

	workloadTypeAzureAppService = "azure_app_service"
	workloadTypeAzureFunction   = "azure_function"

	// envInventoryEnabled gates AAS inventory reporting. Set to "1" alongside
	// DD_AZURE_APP_SERVICES in the Function App / Web App app settings.
	envInventoryEnabled = "DD_SERVERLESS_AAS_EXTENSION_INVENTORY_ENABLED"
)

// IsEnabled reports whether AAS inventory reporting is active.
func IsEnabled() bool {
	return serverlessenv.IsAzureAppServicesExtension() && os.Getenv(envInventoryEnabled) == "1"
}

// NewCapabilities returns the inventoryagent Capabilities for dogstatsd running
// inside the AAS extension: skip cross-process enrichment (no sibling agent
// processes), use a per-process UUID, and force the payload enabled so AAS
// inventory works regardless of the enable_metadata_collection config flag.
func NewCapabilities() *inventoryagent.Capabilities {
	id := uuid.New().String()
	caps := inventoryagent.NewServerlessCapabilities(func() string { return id })
	caps.ForceEnabled = true
	return caps
}

// NewRunnerCapabilities keeps periodic AAS inventory reporting active when the
// standalone dogstatsd configuration disables the full metadata pipeline.
func NewRunnerCapabilities() *metadatarunner.Capabilities {
	return &metadatarunner.Capabilities{ForceEnabled: true}
}

// workloadType returns the downstream workload_type value for this AAS process.
// Azure Function Apps set FUNCTIONS_WORKER_RUNTIME; plain Web Apps do not.
func workloadType() string {
	if _, ok := os.LookupEnv("FUNCTIONS_WORKER_RUNTIME"); ok {
		return workloadTypeAzureFunction
	}
	return workloadTypeAzureAppService
}

// Inject sets the AAS-specific inventory fields on the shared inventoryagent
// component. It returns true when fields were set and Submit should be called.
// It returns false when IsEnabled() is false or when the Azure resource ID
// cannot be derived (required REDAPL key; prevents a dangling row).
//
// Fields use unprefixed names (resource_id, workload_type, …) as required by
// the EPRW decoder.
func Inject(ia inventoryagent.Component, conf configmodel.Reader) bool {
	if !IsEnabled() {
		return false
	}

	aasTags := traceutil.GetAppServicesTags()
	// Azure resource IDs are case-insensitive, while the casing returned by
	// Azure APIs is inconsistent. REDAPL canonicalizes Azure keys to lowercase.
	resourceID := canonicalResourceID(aasTags[traceutil.AASResourceID])
	if resourceID == "" {
		// Cannot form a valid REDAPL key; skip rather than emit a dangling row.
		return false
	}

	ia.Set("flavor", aasInventoryFlavor)
	ia.Set("report_reason", reportReasonStartup)

	ia.Set("resource_id", resourceID)
	ia.Set("resource_name", aasTags[traceutil.AASSiteName])
	ia.Set("workload_type", workloadType())

	ia.Set("region", aasTags[traceutil.AASRegion])
	ia.Set("azure_subscription_id", aasTags[traceutil.AASSubscriptionID])
	ia.Set("azure_resource_group", aasTags[traceutil.AASResourceGroup])
	ia.Set("runtime", aasTags[traceutil.AASRuntime])
	ia.Set("extension_version", aasTags[traceutil.AASExtensionVersion])

	ia.Set("dd_env", conf.GetString("env"))
	ia.Set("dd_site", conf.GetString("site"))
	ia.Set("dd_service", os.Getenv("DD_SERVICE"))
	ia.Set("dd_version", os.Getenv("DD_VERSION"))
	return true
}

func canonicalResourceID(baseResourceID string) string {
	resourceID := strings.ToLower(baseResourceID)
	if resourceID == "" {
		return ""
	}

	slot := strings.TrimSpace(os.Getenv("WEBSITE_SLOT_NAME"))
	if slot != "" && !strings.EqualFold(slot, "production") {
		resourceID += "/slots/" + strings.ToLower(slot)
	}
	return resourceID
}

// Submit builds and enqueues the inventory payload synchronously before the
// metadata runner goroutine fires. It is a no-op when IsEnabled() is false.
// Subsequent periodic submissions are handled by the inventoryagent built-in
// runner (defaultMaxInterval = 10 min), so no separate goroutine is needed.
func Submit(ia inventoryagent.Component) {
	if !IsEnabled() {
		return
	}
	ia.Submit()
	ia.Set("report_reason", reportReasonPeriodic)
}

// Module returns the fx.Option that wires AAS inventory for dogstatsd running
// inside an Azure App Service extension. It provides the Capabilities and
// registers an OnStart hook that injects fields and enqueues the initial payload.
func Module() fx.Option {
	if !IsEnabled() {
		return fx.Options()
	}
	return fx.Options(
		fx.Provide(NewCapabilities),
		fx.Provide(NewRunnerCapabilities),
		fx.Invoke(setupLifecycle),
	)
}

type lifecycleDeps struct {
	fx.In
	Lc             fx.Lifecycle
	InventoryAgent inventoryagent.Component
	Config         coreconfig.Component
}

func setupLifecycle(deps lifecycleDeps) {
	if !IsEnabled() {
		return
	}
	deps.Lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			if Inject(deps.InventoryAgent, deps.Config) {
				Submit(deps.InventoryAgent)
			}
			return nil
		},
	})
}
