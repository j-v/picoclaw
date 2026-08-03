package main

// drill_broken.go — INTENTIONAL syntax error for Phase 7 failure drill.
// Missing closing brace makes `go build ./cmd/picoclaw` fail,
// so the Cross-Compile check fails and the merge is blocked.
// This file is deleted by the drill's auto-fix step.
func drillBroken() {
