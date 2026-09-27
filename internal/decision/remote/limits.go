package remote

const (
	// Jev documented request size cap is 32 KiB
	MaxRequestBodyBytes = 32 * 1024
	// Bound remote response bytes 64 KiB
	MaxResponseBodyBytes = 64 * 1024
	// Max task string length
	MaxTaskLength = 1024
	// Max candidate description length
	MaxCandidateDescriptionLength = 512
	// Max candidates for external provider (bounded)
	MaxCandidates = 100
	// Max priorities
	MaxPriorities = 8
	// Max constraints
	MaxConstraints = 4
)
