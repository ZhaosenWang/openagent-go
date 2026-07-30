// Package aliyun implements provider.CloudProvider for Alibaba Cloud.
//
// This is a stub. It exists to remind us that iac-server is not coupled
// to HuaweiCloud — adding a cloud means implementing this interface +
// providing skills, nothing in server core or iac/ changes.
//
// Catalog and pricing are injected via WithCatalog/WithPricing with real
// SDK/API-backed implementations. Without injection they return nil.
package aliyun

import (
	"os"

	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
)

// Aliyun implements provider.CloudProvider.
type Aliyun struct {
	region  string
	catalog provider.CatalogProvider // injectable; nil = not available
	pricing provider.PricingProvider // injectable; nil = not available
}

// New creates an Aliyun provider for the given region.
// Credentials are read from the environment on demand via Env().
// Catalog and pricing are nil by default — inject real implementations
// via WithCatalog/WithPricing.
func New(region string) *Aliyun {
	return &Aliyun{region: region}
}

// WithCatalog injects a CatalogProvider implementation (e.g. SDK-backed).
// Without this, CatalogProvider() returns nil.
func (a *Aliyun) WithCatalog(c provider.CatalogProvider) *Aliyun {
	a.catalog = c
	return a
}

// WithPricing injects a PricingProvider implementation (e.g. SDK-backed).
// Without this, PricingProvider() returns nil.
// Never inject a static/hardcoded pricing provider — wrong pricing is dangerous.
func (a *Aliyun) WithPricing(p provider.PricingProvider) *Aliyun {
	a.pricing = p
	return a
}

// Name returns the cloud identifier.
func (a *Aliyun) Name() string { return "aliyun" }

// Env returns Aliyun credential environment variables.
// Reads from the process environment at call time so secrets never
// persist in the struct.
func (a *Aliyun) Env() map[string]string {
	return map[string]string{
		"ALICLOUD_ACCESS_KEY": os.Getenv("ALICLOUD_ACCESS_KEY"),
		"ALICLOUD_SECRET_KEY": os.Getenv("ALICLOUD_SECRET_KEY"),
		"ALICLOUD_REGION":     a.region,
	}
}

// CatalogProvider returns the catalog implementation, or nil if not injected.
func (a *Aliyun) CatalogProvider() provider.CatalogProvider {
	return a.catalog
}

// PricingProvider returns the pricing implementation, or nil if not injected.
func (a *Aliyun) PricingProvider() provider.PricingProvider {
	return a.pricing
}
