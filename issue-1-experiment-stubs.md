# PR 1: Register azure-backend experiment + documentation stubs

## Context

Maintainer feedback on [gruntwork-io/terragrunt#4307](https://github.com/gruntwork-io/terragrunt/issues/4307#issuecomment-4354333422) requests breaking the monolithic Azure backend PR (#4487, 155 files / +31K lines) into three incremental, reviewable PRs. This is the first.

A full implementation exists on the `azurerm_storage` branch (`omattsson/terragrunt`) and serves
as **reference only**. Each PR in this series is authored as a fresh branch off `main`, rewriting
the relevant pieces to match Terragrunt's existing patterns (`awshelper`/`gcphelper`, S3/GCS
backends). The existing branch's factory/interfaces/telemetry architecture is intentionally
replaced with the simpler patterns used elsewhere in the codebase.

## Goal

Get the `azure-backend` experiment registered in Terragrunt and documented. **No functional code, no Azure SDK dependencies.** This PR should be trivially reviewable (<200 lines).

## What to do

### 1. Register the experiment in `internal/experiment/experiment.go`

> **Note:** The `azurerm_storage` branch already has this registered (`AzureBackend` constant at
> line 31, `NewExperiments()` entry at line 100). Cherry-pick or rewrite this change.

Add `AzureBackend` to the experiment constants and the `NewExperiments()` factory:

```go
// Line ~31, alongside existing constants:
AzureBackend = "azure-backend"
```

```go
// In NewExperiments(), add to the slice:
{Name: AzureBackend, Status: StatusOngoing},
```

Verify the experiment can be activated with `--experiment azure-backend` and `TG_EXPERIMENT=azure-backend`.

### 2. Add experiment documentation page

> **Note:** Verify the directory `docs/src/content/docs/04-reference/04-experiments/02-active/`
> exists on `main`. If not, check the current experiment docs structure and follow that convention.

Create `docs/src/content/docs/04-reference/04-experiments/02-active/azure-backend.mdx`:

```mdx
---
title: Azure Backend
description: Experimental support for the Azure (azurerm) remote state backend.
slug: reference/experiments/active/azure-backend
sidebar:
  order: 3
---

import { Aside } from '@astrojs/starlight/components';

The `azure-backend` experiment enables native Azure Storage (azurerm) support for
Terragrunt's remote state management. When enabled, Terragrunt can automatically
bootstrap and manage Azure Storage accounts, blob containers, and state files —
matching the existing S3 and GCS backend experience.

## Enabling the experiment

  ```bash
  # Via CLI flag
  terragrunt --experiment azure-backend run -- plan

  # Via environment variable
  export TG_EXPERIMENT=azure-backend
  terragrunt run -- plan
  ```

## Current status

This experiment is under active development. The following capabilities are planned:

- **Bootstrap**: Automatic creation of storage accounts and blob containers
- **Delete**: State blob and container deletion with confirmation prompts
- **Migrate**: SDK-direct blob copy between containers with verification
- **Dependency output fetching**: Direct state file reading from Azure blobs
  (via `--dependency-fetch-output-from-state`)

## Basic configuration

  ```hcl
  remote_state {
    backend = "azurerm"
    config = {
      storage_account_name = "myterragruntstate"
      container_name       = "tfstate"
      key                  = "path/to/terraform.tfstate"
      resource_group_name  = "terraform-rg"
      subscription_id      = "00000000-0000-0000-0000-000000000000"
      use_azuread_auth     = true
    }
  }
  ```

<Aside type="caution">
All Azure backend functionality requires `--experiment azure-backend` or
`TG_EXPERIMENT=azure-backend`. Without the flag, the `azurerm` backend falls
through to OpenTofu/Terraform's native backend handling with no Terragrunt management.
</Aside>

## Authentication methods

| Method | Config fields |
|--------|--------------|
| Azure AD (recommended) | `use_azuread_auth = true` |
| Managed Service Identity | `use_msi = true` |
| Service Principal | `client_id`, `client_secret`, `tenant_id` |
| SAS token | `sas_token` |
| Access key | `access_key` |
```

### 3. Add a stub backend in `internal/remotestate/backend/azurerm/`

Create a minimal backend that registers itself but does nothing functional.

**`backend.go`:**

> **Note:** The existing branch uses `NewBackend(cfg *BackendConfig)` with a config argument.
> This stub uses zero-arg `NewBackend()` to match S3 (`s3.NewBackend()`) and GCS
> (`gcs.NewBackend()`) patterns. The config-arg variant is not needed until PR 3.

```go
package azurerm

import (
    "github.com/gruntwork-io/terragrunt/internal/remotestate/backend"
)

const BackendName = "azurerm"

type Backend struct {
    *backend.CommonBackend
}

func NewBackend() *Backend {
    return &Backend{
        CommonBackend: backend.NewCommonBackend(BackendName),
    }
}

func (b *Backend) Name() string {
    return BackendName
}
```

All other interface methods (`NeedsBootstrap`, `Bootstrap`, `Delete`, etc.) fall through to
`CommonBackend` defaults which log a warning and return nil/false. This is the same pattern
GCS and S3 use for their base.

**`GetTFInitArgs` — passthrough in the stub:**

The stub's `GetTFInitArgs` should pass all config keys through to `terraform init` unchanged.
Full key filtering (removing Terragrunt-only keys like `create_storage_account_if_not_exists`)
is added in PR 3 when the extended config is implemented. Without the experiment enabled,
the backend is a no-op for bootstrap/delete but `GetTFInitArgs` runs unconditionally (it's
called during `terraform init` regardless of experiment state). A passthrough is safe because
Terraform/OpenTofu's native azurerm backend ignores unknown keys.

```go
package azurerm

import "github.com/gruntwork-io/terragrunt/internal/remotestate/backend"

type Config backend.Config

func (cfg Config) GetTFInitArgs() map[string]any {
    result := make(map[string]any, len(cfg))
    for k, v := range cfg {
        result[k] = v
    }
    return result
}
```

### 4. Verify backend registration

In `internal/remotestate/remote_state.go`, the backends list should include:

```go
var backends = backend.Backends{
    s3.NewBackend(),
    gcs.NewBackend(),
    azurerm.NewBackend(),
}
```

### 5. Add minimal state-backend docs section

In `docs/src/content/docs/03-features/01-units/03-state-backend.mdx`, add an Azure section
after GCS with an experimental callout:

```mdx
### Azure Storage (azurerm)

<Aside type="caution" title="Experimental">
Azure backend support requires `--experiment azure-backend`. See the
[experiment documentation](/reference/experiments/active/azure-backend) for details.
</Aside>

  ```hcl
  remote_state {
    backend = "azurerm"
    config = {
      storage_account_name = "myterragruntstate"
      container_name       = "tfstate"
      key                  = "${path_relative_to_include()}/terraform.tfstate"
      resource_group_name  = "terraform-rg"
      subscription_id      = "00000000-0000-0000-0000-000000000000"
      use_azuread_auth     = true
    }
  }
  ```
```

## What NOT to include

- No Azure SDK imports (`github.com/Azure/...`)
- No authentication logic
- No storage account / blob / container operations
- No `internal/azure/` or `internal/azurehelper/` package
- No test files requiring Azure credentials

## Acceptance criteria

- `--experiment azure-backend` activates without error
- `terragrunt --experiment azure-backend run -- plan` with `backend = "azurerm"` passes
  through to Terraform/OpenTofu native azurerm backend (no Terragrunt bootstrap)
- Experiment docs page renders correctly
- State backend docs page has Azure section with experimental callout
- `go build ./...` passes — no Azure SDK dependencies added (only stdlib + existing Terragrunt packages)
- No lint errors

## References

- Maintainer feedback: https://github.com/gruntwork-io/terragrunt/issues/4307#issuecomment-4354333422
- Experiment system: [`internal/experiment/experiment.go`](internal/experiment/experiment.go)
- Existing experiment docs: `docs/src/content/docs/04-reference/04-experiments/02-active/`
- S3 backend stub pattern: [`internal/remotestate/backend/s3/backend.go`](internal/remotestate/backend/s3/backend.go)
