// Package-level DAG contract for iac-server deployments.
//
// The DAG is the structured contract that flows through the deployment
// steps: propose_architecture creates it, specify_resources enriches node
// specs, generate_terraform_plan consumes it to write .tf files,
// estimate_cost prices it, apply_deployment gates on it. It is persisted
// as dag.json in the deployment directory — state lives on disk, not in
// conversation history, so context compression can never lose it.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DagVersion is the current dag.json schema version. loadDag rejects files
// with an unknown version so a future schema change fails loudly instead of
// misreading the structure.
const DagVersion = 1

// CostVersion is the current cost.json schema version.
const CostVersion = 1

// DagStatus tracks how far a deployment has progressed. Steps enforce a
// minimum status (e.g. generate requires "specified") by checking the DAG
// they load.
type DagStatus string

const (
	DagProposed      DagStatus = "proposed"       // propose_architecture
	DagSpecified     DagStatus = "specified"      // specify_resources
	DagPlanned       DagStatus = "planned"        // generate_terraform_plan
	DagCostEstimated DagStatus = "cost_estimated" // estimate_cost
	DagApplied       DagStatus = "applied"        // apply_deployment
	DagDestroyed     DagStatus = "destroyed"      // destroy_deployment
)

// maxDagNodes bounds the DAG size to keep LLM context and validation cheap.
const maxDagNodes = 50

// Dag is the deployment graph: nodes are resources, dependencies are
// embedded per-node as depends_on (node ids).
type Dag struct {
	Version      int       `json:"version"`
	DeploymentID string    `json:"deployment_id"`
	Architecture string    `json:"architecture,omitempty"`
	Region       string    `json:"region,omitempty"`
	Status       DagStatus `json:"status"`
	UpdatedAt    string    `json:"updated_at"`
	Nodes        []DagNode `json:"nodes"`
}

// DagNode is a single resource. ID is a short stable id assigned by the
// LLM (e.g. "ecs-web"); Type + Name form the terraform address type.name.
// Spec holds the concrete resource specification (flavor, image, disk,
// CIDR, ...) filled by specify_resources.
type DagNode struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	Spec      map[string]any `json:"spec,omitempty"`
	DependsOn []string       `json:"depends_on,omitempty"`
}

// CostEstimate is the persisted estimate_cost result. apply_deployment
// gates on the existence of cost.json — any DAG or .tf mutation deletes
// it via invalidateCost, forcing a re-estimate.
type CostEstimate struct {
	Version      int    `json:"version"`
	DeploymentID string `json:"deployment_id"`
	PricingMode  string `json:"pricing_mode"` // "on-demand" | "monthly"
	EstimatedAt  string `json:"estimated_at"`
	Items        []any  `json:"items"`
	TotalMonthly any    `json:"total_monthly"`
	Currency     string `json:"currency"`
	Note         string `json:"note"`
}

// dagPath returns the dag.json path inside a deployment directory.
func dagPath(dir string) string {
	return filepath.Join(dir, "dag.json")
}

// costPath returns the cost.json path inside a deployment directory.
func costPath(dir string) string {
	return filepath.Join(dir, "cost.json")
}

// loadDag reads and validates the DAG for a deployment. The error messages
// are part of the client-facing contract (greppable by client tests).
func loadDag(dir string) (*Dag, error) {
	data, err := os.ReadFile(dagPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("deployment %s has no dag.json — call propose_architecture first (or update_deployment with a change request for pre-DAG deployments)", filepath.Base(dir))
		}
		return nil, fmt.Errorf("read dag.json: %w", err)
	}
	var dag Dag
	if err := json.Unmarshal(data, &dag); err != nil {
		return nil, fmt.Errorf("parse dag.json: %w", err)
	}
	if err := validateDag(&dag); err != nil {
		return nil, err
	}
	return &dag, nil
}

// validateDag checks schema version, node id uniqueness, depends_on
// references, and acyclicity (Kahn's algorithm). Tolerant unmarshal:
// unknown fields are ignored, missing depends_on defaults to [].
func validateDag(dag *Dag) error {
	if dag.Version != DagVersion {
		return fmt.Errorf("dag.json schema version %d not supported — upgrade iac-server", dag.Version)
	}
	if len(dag.Nodes) == 0 {
		return fmt.Errorf("dag.json has no nodes")
	}
	if len(dag.Nodes) > maxDagNodes {
		return fmt.Errorf("dag.json has %d nodes — max is %d", len(dag.Nodes), maxDagNodes)
	}
	ids := make(map[string]bool, len(dag.Nodes))
	for _, n := range dag.Nodes {
		if n.ID == "" {
			return fmt.Errorf("dag.json node with empty id")
		}
		if ids[n.ID] {
			return fmt.Errorf("dag.json duplicate node id %q", n.ID)
		}
		ids[n.ID] = true
	}
	for _, n := range dag.Nodes {
		for _, dep := range n.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("dag.json node %q depends on unknown node %q", n.ID, dep)
			}
		}
	}
	// Cycle detection: Kahn's algorithm on the id graph.
	indeg := make(map[string]int, len(dag.Nodes))
	for _, n := range dag.Nodes {
		seen := make(map[string]bool)
		for _, dep := range n.DependsOn {
			if seen[dep] {
				return fmt.Errorf("dag.json node %q lists dependency %q more than once", n.ID, dep)
			}
			seen[dep] = true
			indeg[n.ID]++
		}
	}
	queue := []string{}
	for _, n := range dag.Nodes {
		if indeg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, n := range dag.Nodes {
			if contains(n.DependsOn, id) {
				indeg[n.ID]--
				if indeg[n.ID] == 0 {
					queue = append(queue, n.ID)
				}
			}
		}
	}
	if visited != len(dag.Nodes) {
		return fmt.Errorf("dag.json contains a dependency cycle")
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// saveDag writes the DAG to disk with an updated timestamp.
func saveDag(dir string, dag *Dag) error {
	dag.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(dag, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dag.json: %w", err)
	}
	if err := os.WriteFile(dagPath(dir), data, 0644); err != nil {
		return fmt.Errorf("write dag.json: %w", err)
	}
	return nil
}

// dagInput serializes the DAG compactly for embedding in an LLM user
// message. Nodes are sorted by id for stable output across steps.
func dagInput(dag *Dag) (string, error) {
	sorted := make([]DagNode, len(dag.Nodes))
	copy(sorted, dag.Nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	clone := *dag
	clone.Nodes = sorted
	data, err := json.Marshal(&clone)
	if err != nil {
		return "", fmt.Errorf("marshal dag for input: %w", err)
	}
	return string(data), nil
}

// nodeSpecsFilled reports whether every node has a non-empty spec — the
// gate for generate_terraform_plan and estimate_cost.
func nodeSpecsFilled(dag *Dag) bool {
	for _, n := range dag.Nodes {
		if len(n.Spec) == 0 {
			return false
		}
	}
	return len(dag.Nodes) > 0
}

// saveCost persists an estimate_cost result.
func saveCost(dir string, c *CostEstimate) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cost.json: %w", err)
	}
	if err := os.WriteFile(costPath(dir), data, 0644); err != nil {
		return fmt.Errorf("write cost.json: %w", err)
	}
	return nil
}

// HasCost reports whether a valid estimate marker exists for the
// deployment. apply_deployment gates on this.
func HasCost(dir string) bool {
	_, err := os.Stat(costPath(dir))
	return err == nil
}

// invalidateCost deletes the estimate marker. Any DAG or .tf mutation
// invalidates the previous estimate, forcing a re-estimate before apply.
func invalidateCost(dir string) error {
	if err := os.Remove(costPath(dir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("invalidate cost.json: %w", err)
	}
	return nil
}
