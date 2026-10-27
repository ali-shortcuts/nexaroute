package nexaflow

// allNodeSpecs lists every adapter node in registration order. The count
// is asserted by TestRegistryNodeCount; add a node here and in the docs
// table together, never one without the other.
func allNodeSpecs() []*NodeSpec {
	specs := []*NodeSpec{}
	specs = append(specs, llmNodeSpecs()...)
	specs = append(specs, providerNodeSpecs()...)
	specs = append(specs, evalNodeSpecs()...)
	specs = append(specs, mcpNodeSpecs()...)
	specs = append(specs, videoNodeSpecs()...)
	specs = append(specs, costNodeSpecs()...)
	specs = append(specs, triggerNodeSpecs()...)
	specs = append(specs, controlNodeSpecs()...)
	return specs
}
