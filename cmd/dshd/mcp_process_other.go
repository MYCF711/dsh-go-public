//go:build !windows

package main

import "os/exec"

type mcpProcessTree struct{}

func attachMCPProcessTree(*exec.Cmd) (*mcpProcessTree, error) { return &mcpProcessTree{}, nil }
func (*mcpProcessTree) Stop() error                           { return nil }
func (*mcpProcessTree) Close() error                          { return nil }
