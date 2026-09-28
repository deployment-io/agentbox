package agent

// VerifyReadOnlyMountsForTest exposes the write probe to this package's
// EXTERNAL test package (package agent_test), which is the only place a test
// can import an agent driver: every driver imports internal/agent, so an
// in-package test that imported one back would be an import cycle.
//
// It exists for the one assertion that cannot be made anywhere else — that the
// probe's verdict and the driver's tool allowlist agree — and is compiled only
// into the test binary, so it widens no API.
var VerifyReadOnlyMountsForTest = verifyReadOnlyMounts
