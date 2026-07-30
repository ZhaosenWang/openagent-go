// Package provider defines the CloudProvider interface and related types
// for abstracting cloud-specific catalog, pricing, and credentials.
//
// Templates are NOT part of this package — they come from skills.
// A skill directory contains SKILL.md (usage docs) + *.tf.tmpl (templates),
// provided by the cloud vendor, community, or user. The server loads skills
// via SkillLoader and generates .tf files via template.Generator.
//
// Adding a cloud = implementing CloudProvider + providing skills.
// Adding a deployment pattern = adding a skill directory.
// Neither requires changes to server core or iac/.
package provider

import (
	"context"
)

// CloudProvider provides cloud identity, credentials, and swappable
// catalog/pricing capabilities. Templates are deliberately excluded —
// they come from skills, not from the provider implementation.
//
// CatalogProvider and PricingProvider may return nil if no implementation
// is configured. Callers must nil-check before use.
type CloudProvider interface {
	// Name returns the cloud identifier, e.g. "huaweicloud", "aliyun".
	Name() string

	// Env returns cloud credential environment variables (e.g. HW_ACCESS_KEY).
	// Read from the process environment at call time so secrets never
	// persist in the struct.
	Env() map[string]string

	// CatalogProvider returns the catalog implementation, or nil.
	// Implementations query cloud APIs/SDKs for available flavors and zones.
	CatalogProvider() CatalogProvider

	// PricingProvider returns the pricing implementation, or nil.
	// Implementations MUST query real cloud pricing APIs/SDKs — never
	// hardcoded estimates. Wrong pricing is dangerous.
	// Returns nil when no pricing backend is configured; callers should
	// nil-check and fall back to LLM-driven tool calls to query pricing.
	PricingProvider() PricingProvider
}

// CatalogProvider queries available resource flavors and availability zones.
// Implementations should query cloud APIs/SDKs for live data.
// A nil CatalogProvider means catalog is unavailable.
type CatalogProvider interface {
	Catalog(ctx context.Context, query CatalogQuery) (*CatalogResult, error)
}

// PricingProvider queries the monthly cost for a set of resources.
// Implementations MUST query real cloud pricing APIs/SDKs.
// Never use hardcoded estimates — prices change, wrong pricing is dangerous.
// A nil PricingProvider means pricing is unavailable.
type PricingProvider interface {
	Pricing(ctx context.Context, resources []ResourceSpec) (*PriceResult, error)
}

// ResourceSpec describes a resource in cloud-agnostic terms.
// Only fields common to ALL cloud resources are top-level; everything
// resource-specific lives in Props and is accessed by templates via
// {{ .Props.xxx }}.
//
// Examples:
//   ECS:  Type="ecs", Props={"flavor":"s7.large.2", "count":2, "az":"cn-east-3a", "public_ip":true}
//   RDS:  Type="rds", Props={"flavor":"rds.mysql.s3", "engine":"mysql", "version":"8.0", "disk_size":100}
//   OBS:  Type="obs", Props={"storage_class":"STANDARD", "versioning":true}
//   VPC:  Type="vpc", Props={"cidr":"192.168.0.0/16"}
type ResourceSpec struct {
	Type   string         `json:"type"`   // resource type, e.g. "ecs", "rds", "obs", "vpc" (skill-defined)
	Name   string         `json:"name"`   // resource label, used for .tf filename and cross-refs
	Region string         `json:"region"` // deployment region, e.g. "cn-east-3"
	Props  map[string]any `json:"props"`  // all resource-specific attributes (flavor, count, az, engine, ...)
}

// CatalogQuery selects which catalog entries to return.
type CatalogQuery struct {
	Region       string `json:"region"`        // required
	ResourceType string `json:"resource_type"` // "ecs", "rds", "" = all
}

// CatalogResult lists available flavors and zones for a region.
type CatalogResult struct {
	Region  string   `json:"region"`
	AZs     []string `json:"azs"`     // available zones, e.g. ["cn-north-4a", "cn-north-4b"]
	Flavors []Flavor `json:"flavors"` // available specs
}

// Flavor is a single resource specification available in the catalog.
// Specs holds arbitrary dimensions (cpu, memory, disk_type, ...) filled
// by the CatalogProvider implementation — no ECS-centric assumptions.
type Flavor struct {
	ID    string         `json:"id"`    // cloud-specific spec ID, e.g. "s7.large.2"
	Specs map[string]any `json:"specs"` // arbitrary dimensions: {cpu:2, memory:4, ...}
	AZs   []string       `json:"azs"`   // zones where this flavor is available
}

// PriceResult is the pricing breakdown for a set of resources.
// Prices MUST come from a real source (cloud pricing API, SDK, etc.).
// Never use hardcoded estimates — wrong pricing is dangerous.
type PriceResult struct {
	Items []PriceItem `json:"items"`
	Total float64     `json:"total"` // monthly total in CNY
}

// PriceItem is the cost contribution of a single resource.
// Count is only meaningful for resources that have multiplicity
// (e.g. compute instances); for singular resources (OBS bucket, ELB)
// the implementation should leave it as 0 or 1.
type PriceItem struct {
	Resource string  `json:"resource"` // cloud-specific resource descriptor
	Count    int     `json:"count"`    // instance count, if applicable
	Monthly  float64 `json:"monthly"`  // unit price CNY/month
	Subtotal float64 `json:"subtotal"` // Monthly * Count
}
