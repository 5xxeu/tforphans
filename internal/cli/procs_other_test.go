//go:build !unix

package cli

func killDescendants(int) error { return nil }
