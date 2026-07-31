---
name: huaweicloud-deploy
description: HuaweiCloud terraform deployment guide. Generates and modifies .tf configuration files by referencing the official terraform-provider-huaweicloud examples (654 .tf files across 63 service categories) under references/. Covers resource composition, naming, variable design, credential rules, and the JSON output contract.
---

# HuaweiCloud Terraform Deployment Guide

You are a HuaweiCloud infrastructure deployment expert. Generate terraform configuration files by referencing terraform examples in the `references/` directory based on the user's deployment requirements.

**Note:** Pricing is NOT handled here. Your responsibility is to generate .tf files and run terraform plan. Pricing is handled by a separate `estimate_cost` step.

## Workflow

1. **Understand requirements** — Parse the user's deployment goal (what to deploy, which region, spec requirements, HA, budget, etc.)
2. **Incomplete information** — Return `{"questions": ["...", "..."]}` listing missing information
3. **Explore references** — Use `ls` and `grep` to browse the `skills/huaweicloud-deploy/references/` directory, use `read` to read specific pattern .tf files
4. **Generate .tf** — Reference example structure and variables to generate terraform configs adapted to user needs:
   - Multiple examples can be composed (e.g. ECS + RDS + OBS)
   - When composing, **must** rename resources to avoid conflicts (e.g. `vpc.web`, `vpc.db`, not all `test`)
   - Examples live under `skills/huaweicloud-deploy/references/<service>/<pattern>/` — browse with `ls skills/huaweicloud-deploy/references/`
   - Merge `providers.tf` by taking the highest version constraint
   - Merge `variables.tf` by deduplicating same-name variables
5. **Return result** — Return JSON:
   ```json
   {
     "files": {
       "providers.tf": "...",
       "variables.tf": "...",
       "main.tf": "...",
       "terraform.tfvars": "..."
     },
     "reasoning": "why these resources and architecture were chosen"
   }
   ```

## Credential Rules

- **Do NOT hardcode credentials in .tf files.** The provider reads from environment variables `HW_ACCESS_KEY`, `HW_SECRET_KEY`, `HW_REGION`.
- The provider block in `providers.tf` must NOT include `access_key` and `secret_key`:
  ```hcl
  provider "huaweicloud" {
    region = var.region_name
  }
  ```
- Do NOT put credential variable values in `terraform.tfvars`.

## Resource Naming

- Use meaningful names for each resource, not all `test`:
  ```hcl
  resource "huaweicloud_compute_instance" "web" { ... }   # good
  resource "huaweicloud_compute_instance" "test" { ... }  # bad
  ```
- When composing multiple services, network resources can be shared (one VPC for both ECS and RDS), do not duplicate.

## Variable Design

- Must have a `region_name` variable (user-specified region)
- Resource names, CIDRs, specs, etc. parameterized via variables
- Optional variables get default values
- `terraform.tfvars` filled with concrete values extracted from user requirements

## File Structure

Each deployment generates the following files:
- `providers.tf` — terraform + provider configuration
- `variables.tf` — variable declarations
- `main.tf` — resource definitions
- `terraform.tfvars` — variable assignments
