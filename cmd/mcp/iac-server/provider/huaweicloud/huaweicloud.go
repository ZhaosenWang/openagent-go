// Package huaweicloud implements provider.CloudProvider for HuaweiCloud.
//
// Catalog and pricing are NOT hardcoded — they are injected via
// WithCatalog/WithPricing with real SDK/API-backed implementations.
// Without injection, CatalogProvider()/PricingProvider() return nil
// and the server LLM should query cloud APIs via tool calls instead.
//
// Templates are NOT provided here — they come from skills.
package huaweicloud

import (
	"os"

	"github.com/yusheng-g/openagent-go/cmd/mcp/iac-server/provider"
)

// HuaweiCloud implements provider.CloudProvider.
type HuaweiCloud struct {
	region  string
	catalog provider.CatalogProvider // injectable; nil = not available
	pricing provider.PricingProvider // injectable; nil = not available
}

// New creates a HuaweiCloud provider for the given region.
// Credentials are read from the environment on demand via Env().
// Catalog and pricing are nil by default — inject real implementations
// via WithCatalog/WithPricing.
func New(region string) *HuaweiCloud {
	return &HuaweiCloud{region: region}
}

// WithCatalog injects a CatalogProvider implementation (e.g. SDK-backed).
// Without this, CatalogProvider() returns nil.
func (h *HuaweiCloud) WithCatalog(c provider.CatalogProvider) *HuaweiCloud {
	h.catalog = c
	return h
}

// WithPricing injects a PricingProvider implementation (e.g. SDK-backed).
// Without this, PricingProvider() returns nil.
// Never inject a static/hardcoded pricing provider — wrong pricing is dangerous.
func (h *HuaweiCloud) WithPricing(p provider.PricingProvider) *HuaweiCloud {
	h.pricing = p
	return h
}

// Name returns the cloud identifier.
func (h *HuaweiCloud) Name() string { return "huaweicloud" }

// Env returns HuaweiCloud credential environment variables.
// Reads from the process environment at call time so secrets never
// persist in the struct.
func (h *HuaweiCloud) Env() map[string]string {
	return map[string]string{
		"HW_ACCESS_KEY": os.Getenv("HW_ACCESS_KEY"),
		"HW_SECRET_KEY": os.Getenv("HW_SECRET_KEY"),
		"HW_REGION":     h.region,
	}
}

// CatalogProvider returns the catalog implementation, or nil if not injected.
func (h *HuaweiCloud) CatalogProvider() provider.CatalogProvider {
	return h.catalog
}

// PricingProvider returns the pricing implementation, or nil if not injected.
func (h *HuaweiCloud) PricingProvider() provider.PricingProvider {
	return h.pricing
}
