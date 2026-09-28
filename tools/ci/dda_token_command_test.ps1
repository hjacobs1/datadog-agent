# [TEST] Validates DDA_FEATURE_FLAGS_CI_TOKEN_COMMAND with dda from DataDog/datadog-agent-dev#310. Drop before merge.
$ErrorActionPreference = "Stop"

python -m pip install "git+https://github.com/DataDog/datadog-agent-dev.git@$env:DDA_TEST_REF"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

dda --version
$output = dda self feature agent-ci-gitlab-short-lived-tokens --json
Write-Host $output
$result = $output | ConvertFrom-Json
if ($null -ne $result.error -or $result.defaulted) { exit 1 }
